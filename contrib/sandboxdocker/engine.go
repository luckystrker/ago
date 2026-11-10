package sandboxdocker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultHost is the Docker daemon address used when neither WithHost nor the
// DOCKER_HOST environment variable names one.
const DefaultHost = "unix:///var/run/docker.sock"

// APIError is an error response of the Docker Engine API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker API error %d: %s", e.Status, e.Message) }

// ErrNotFound matches (errors.Is) an APIError with status 404 and a missing
// file in an archive download.
var ErrNotFound = errors.New("sandboxdocker: not found")

// Is lets errors.Is(err, ErrNotFound) match a 404.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// engine is a minimal client of the Docker Engine HTTP API over a unix socket
// or TCP (stdlib only; the Python port uses docker-py).
//
// AGO-DEVIATION: TLS endpoints (DOCKER_TLS_VERIFY, https://) and ssh:// hosts
// are not supported; the daemon is reached over a unix socket or plain tcp://.
type engine struct {
	http *http.Client
	base string // http://docker for a unix socket, http://host:port for tcp
}

// newEngine builds a client for host (unix:///path, unix://path, /path,
// tcp://host:port or http://host:port).
func newEngine(host string) (*engine, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	switch {
	case strings.HasPrefix(host, "unix://"), strings.HasPrefix(host, "/"):
		sock := strings.TrimPrefix(host, "unix://")
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			DisableCompression: true,
		}
		return &engine{http: &http.Client{Transport: tr}, base: "http://docker"}, nil
	case strings.HasPrefix(host, "tcp://"), strings.HasPrefix(host, "http://"):
		addr := strings.TrimPrefix(strings.TrimPrefix(host, "tcp://"), "http://")
		return &engine{http: &http.Client{Transport: &http.Transport{DisableCompression: true}}, base: "http://" + addr}, nil
	}
	return nil, fmt.Errorf("sandboxdocker: unsupported docker host %q (want unix://, tcp:// or http://)", host)
}

func (e *engine) close() { e.http.CloseIdleConnections() }

// do sends one request. A non-2xx status becomes an *APIError (the daemon's
// {"message": ...} body is unwrapped); ok statuses are returned open for the
// caller to read and close.
func (e *engine) do(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) (*http.Response, error) {
	u := e.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
		msg = parsed.Message
	}
	return nil, &APIError{Status: resp.StatusCode, Message: msg}
}

// call sends a JSON request and decodes a JSON response into out (nil = ignore).
func (e *engine) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := e.do(ctx, method, path, query, ct, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// createRequest is the body of POST /containers/create.
type createRequest struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir"`
	User       string            `json:"User,omitempty"`
	HostConfig createHostConfig  `json:"HostConfig"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type createHostConfig struct {
	NetworkMode string   `json:"NetworkMode"`
	Memory      int64    `json:"Memory,omitempty"`
	CPUQuota    int64    `json:"CpuQuota,omitempty"`
	AutoRemove  bool     `json:"AutoRemove"`
	Binds       []string `json:"Binds,omitempty"`
}

// createContainer creates and starts a container. Like docker-py's
// containers.run (docker/models/containers.py: `except ImageNotFound:
// self.client.images.pull(image); self.create(...)`) it pulls the image and
// retries once when the daemon answers 404 to the create.
func (e *engine) createContainer(ctx context.Context, req createRequest) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := e.call(ctx, http.MethodPost, "/containers/create", nil, req, &created)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		if perr := e.pullImage(ctx, req.Image); perr != nil {
			return "", perr
		}
		created.ID = ""
		err = e.call(ctx, http.MethodPost, "/containers/create", nil, req, &created)
	}
	if err != nil {
		return "", err
	}
	if err := e.call(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil); err != nil {
		var sErr *APIError
		if !(errors.As(err, &sErr) && sErr.Status == http.StatusNotModified) {
			return "", err
		}
	}
	return created.ID, nil
}

// pullImage pulls image (name[:tag]) and waits for the stream to finish.
func (e *engine) pullImage(ctx context.Context, image string) error {
	repo, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") && i >= 0 {
		repo, tag = image[:i], image[i+1:]
	}
	resp, err := e.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {repo}, "tag": {tag}}, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body is a stream of JSON progress objects; an "error" member is a
	// pull failure the HTTP status cannot carry.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("sandboxdocker: pull %s: %s", image, msg.Error)
		}
	}
}

// stopContainer stops a container within grace seconds; an already stopped or
// removed container is not an error.
func (e *engine) stopContainer(ctx context.Context, id string, grace int) error {
	err := e.call(ctx, http.MethodPost, "/containers/"+id+"/stop", url.Values{"t": {fmt.Sprint(grace)}}, nil, nil)
	return ignoreStatus(err, http.StatusNotFound, http.StatusNotModified)
}

// removeContainer force-removes a container; a missing one is not an error.
func (e *engine) removeContainer(ctx context.Context, id string) error {
	err := e.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"true"}}, nil, nil)
	return ignoreStatus(err, http.StatusNotFound, http.StatusConflict)
}

func ignoreStatus(err error, statuses ...int) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		for _, s := range statuses {
			if apiErr.Status == s {
				return nil
			}
		}
	}
	return err
}

// execRequest is the body of POST /containers/{id}/exec.
type execRequest struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	WorkingDir   string   `json:"WorkingDir"`
	Env          []string `json:"Env,omitempty"`
}

// exec runs cmd in the container and returns the combined stdout+stderr stream
// (frame order preserved, at most limit bytes kept, the rest counted) with the
// exit code.
func (e *engine) exec(ctx context.Context, id string, req execRequest, limit int) (out []byte, dropped int64, exitCode int, err error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err = e.call(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, req, &created); err != nil {
		return nil, 0, 0, err
	}
	b, _ := json.Marshal(map[string]bool{"Detach": false, "Tty": false})
	resp, err := e.do(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, 0, 0, err
	}
	out, dropped, err = readMultiplexed(resp.Body, limit)
	resp.Body.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	var info struct {
		ExitCode int `json:"ExitCode"`
	}
	if err = e.call(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
		return nil, 0, 0, err
	}
	return out, dropped, info.ExitCode, nil
}

// readMultiplexed reads the Docker attach stream: frames of an 8-byte header
// (stream type, three zero bytes, big-endian payload size) and the payload.
// stdout and stderr are returned in arrival order, which is the combined
// output docker-py gives for a non-TTY exec. Bytes beyond limit are read and
// counted but not kept, so a chatty process cannot grow memory without bound.
func readMultiplexed(r io.Reader, limit int) (kept []byte, dropped int64, err error) {
	var header [8]byte
	var buf bytes.Buffer
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), dropped, nil
			}
			return nil, 0, fmt.Errorf("sandboxdocker: read exec stream: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		room := int64(limit) - int64(buf.Len())
		if room < 0 {
			room = 0
		}
		take := size
		if take > room {
			take = room
		}
		if _, err := io.CopyN(&buf, r, take); err != nil {
			return nil, 0, fmt.Errorf("sandboxdocker: read exec stream: %w", err)
		}
		if skip := size - take; skip > 0 {
			if _, err := io.CopyN(io.Discard, r, skip); err != nil {
				return nil, 0, fmt.Errorf("sandboxdocker: read exec stream: %w", err)
			}
			dropped += skip
		}
	}
}

// putFile uploads one file (and its parent directories) as a tar archive
// extracted at dir inside the container.
func (e *engine) putFile(ctx context.Context, id, dir, rel string, content []byte) error {
	archive, err := singleFileTar(rel, content)
	if err != nil {
		return err
	}
	resp, err := e.do(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// getFile downloads one file: the daemon answers with a tar stream whose first
// regular entry is the file.
func (e *engine) getFile(ctx context.Context, id, target string, limit int64) ([]byte, error) {
	resp, err := e.do(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {target}}, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: %s is not a regular file", ErrNotFound, target)
			}
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("sandboxdocker: %s exceeds the %d-byte download limit", target, limit)
		}
		return b, nil
	}
}

// singleFileTar builds a tar with the directory entries leading to rel and the
// file itself.
func singleFileTar(rel string, content []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/") + "/"
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now}); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)), ModTime: now}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
