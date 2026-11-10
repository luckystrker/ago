// Package toolpacks carries the ready-made tools built on the ago/tools
// framework: a local shell tool, a local code runner, a filesystem toolkit,
// web-search tools, a skills loader and a dynamic-agent factory.
//
// Parity with Python AG2 ag2/tools/ (shell/, code/ + sandbox/local.py,
// toolkits/filesystem.py, search/, skills/, dynamic/factory.py). The port is a
// functional subset: the Docker and Daytona sandboxes (contrib modules on the
// Sandbox seam of sandbox.go), provider-hosted tool flags and the in-process
// script runtime of skills are deferred — each deviation carries an
// AGO-DEVIATION comment and a docs/DECISIONS.md entry.
package toolpacks
