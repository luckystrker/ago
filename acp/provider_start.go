package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/tools"
)

// startSession launches the agent, performs the handshake and creates the ACP
// session. A failure at any step leaves no subprocess and no tool host.
func (c *Config) startSession(ctx context.Context, functions []tools.Schema, external []MCPServer) (_ *providerSession, err error) {
	s := &providerSession{cfg: c}
	name, args, env := c.launchSpec()
	cwd := c.cwd()
	fsRoot := c.FSRoot
	if fsRoot == "" {
		fsRoot = cwd
	}
	b := &Bridge{
		Permission:        s.permission,
		OnUpdate:          s.onUpdate,
		FS:                &FSConfig{Root: fsRoot},
		dir:               cwd,
		advertiseTerminal: !c.DisableTerminal,
	}
	b.Elicit = s.elicit
	s.b = b

	defer func() {
		if err != nil {
			if s.host != nil {
				s.host.close()
			}
			if s.b != nil {
				s.b.kill()
				_ = s.b.Close()
			}
		}
	}()

	var mcp []MCPServer
	if !c.DisableTools {
		if len(functions) > 0 {
			for _, e := range external {
				if e.Name == gatewayServerName {
					return nil, fmt.Errorf("%w: MCPServerTool server_label %q collides with the name AGo uses for "+
						"its own tool host in mcp_servers; rename that server", config.ErrUnsupportedTool, gatewayServerName)
				}
			}
			host, herr := newToolHost(functions, s.onChannelFailure)
			if herr != nil {
				return nil, herr
			}
			if herr := host.start(); herr != nil {
				return nil, herr
			}
			s.host = host
			mcp = append(mcp, host.declaration())
		}
		mcp = append(mcp, external...)
		s.external = external
	}

	// The handshake and session/new are bounded by the startup timeout and
	// the caller's context: either one kills the process, which unblocks the
	// exchange parked on it.
	startup := c.StartupTimeout
	if startup <= 0 {
		startup = defaultStartupTimeout
	}
	hsDone := make(chan struct{})
	b.afterStart = func() {
		go func() {
			timer := time.NewTimer(startup)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				b.kill()
			case <-timer.C:
				b.kill()
			case <-hsDone:
			}
		}()
	}
	startErr := b.start(name, args, env)
	if startErr == nil {
		var info SessionInfo
		if len(mcp) > 0 && !b.Caps.MCPHTTP {
			startErr = fmt.Errorf("%w: agent %q did not advertise mcpCapabilities.http, so AGo cannot expose "+
				"the agent's tools to it; remove the tools or set DisableTools", ErrMCPCapability, agentName(b, c))
		} else {
			info, startErr = b.NewSession(SessionParams{
				CWD: cwd, AdditionalDirectories: c.AdditionalDirectories, MCPServers: mcp,
			})
		}
		if startErr == nil {
			startErr = s.selectModel(b, info)
		}
		if startErr == nil {
			s.id = info.ID
		}
	}
	close(hsDone)
	if startErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("acp: starting the agent: %w", cerr)
		}
		if errors.Is(startErr, ErrAgentExited) || errors.Is(startErr, ErrClosed) {
			return nil, &TransportError{Transport: "stdio", Err: startErr}
		}
		return nil, startErr
	}
	return s, nil
}

func agentName(b *Bridge, c *Config) string {
	if b.Info.Name != "" {
		return b.Info.Name
	}
	return c.agentLabel()
}

// selectModel applies Config.ModelName through session/set_config_option and
// records the model the agent reports running (Python parity:
// session.select_model + current_model).
func (s *providerSession) selectModel(b *Bridge, info SessionInfo) error {
	want := s.cfg.ModelName
	opt := info.ModelOption
	if want != "" && opt != nil && opt.CurrentValue != want {
		offered := false
		for _, v := range opt.Values {
			if v == want {
				offered = true
				break
			}
		}
		if !offered {
			return fmt.Errorf("%w: %q (%d offered)%s",
				ErrModelNotOffered, want, len(opt.Values), closestModels(want, opt.Values))
		}
		if err := b.SetConfigOption(info.ID, opt.ID, want); err != nil {
			return err
		}
	}
	// Without a model picker the requested model stays response metadata.
	switch {
	case want != "":
		s.model = want
	case opt != nil:
		s.model = opt.CurrentValue
	}
	return nil
}

// closestModels names up to three offered ids resembling want, for the error
// text.
func closestModels(want string, values []string) string {
	var close []string
	lw := strings.ToLower(want)
	for _, v := range values {
		if lv := strings.ToLower(v); strings.Contains(lv, lw) || strings.Contains(lw, lv) {
			close = append(close, v)
			if len(close) == 3 {
				break
			}
		}
	}
	if len(close) == 0 {
		return ""
	}
	return fmt.Sprintf("; closest offered: %v", close)
}
