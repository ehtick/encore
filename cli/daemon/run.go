package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/logrusorgru/aurora/v3"
	"github.com/rs/zerolog/log"

	"encr.dev/cli/daemon/run"
	"encr.dev/internal/optracker"
	"encr.dev/internal/userconfig"
	"encr.dev/internal/version"
	"encr.dev/pkg/fns"
	daemonpb "encr.dev/proto/encore/daemon"
)

// Run runs the application.
func (s *Server) Run(req *daemonpb.RunRequest, stream daemonpb.Daemon_RunServer) error {
	ctx := stream.Context()
	slog := &streamLog{stream: stream, buffered: true}
	stderr := slog.Stderr(false)

	sendExit := func(code int32) {
		_ = stream.Send(&daemonpb.CommandMessage{
			Msg: &daemonpb.CommandMessage_Exit{Exit: &daemonpb.CommandExit{
				Code: code,
			}},
		})
	}

	userConfig, err := userconfig.ForApp(req.AppRoot).Get()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("failed to load config: %v"), err))
		sendExit(1)
		return nil
	}

	ctx, tracer, err := s.beginTracing(ctx, req.AppRoot, req.WorkingDir, req.TraceFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("failed to begin tracing: %v"), err))
		sendExit(1)
		return nil
	}
	defer fns.CloseIgnore(tracer)

	// ListenAddr should always be passed but guard against old clients.
	listenAddr := req.ListenAddr
	if listenAddr == "" {
		listenAddr = ":4000"
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		if errIsAddrInUse(err) {
			_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("Failed to run on %s - port is already in use"), listenAddr))
		} else {
			_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("Failed to run on %s - %v"), listenAddr, err))
		}

		if host, port, ok := findAvailableAddr(listenAddr); ok {
			if host == "localhost" || host == "127.0.0.1" {
				_, _ = fmt.Fprintf(stderr, "Note: port %d is available; specify %s to use it\n",
					port, aurora.Sprintf(aurora.Cyan("--port=%d"), port))
			} else {
				_, _ = fmt.Fprintf(stderr, "Note: address %s:%d is available; specify %s to use it\n",
					host, port, aurora.Sprintf(aurora.Cyan("--listen=%s:%d"), host, port))
			}
		} else {
			_, _ = fmt.Fprintf(stderr, "Note: specify %s to run on another port\n",
				aurora.Cyan("--port=NUMBER"))
		}

		sendExit(1)
		return nil
	}
	defer fns.CloseIgnore(ln)

	app, err := s.apps.Track(req.AppRoot)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("failed to resolve app: %v"), err))
		sendExit(1)
		return nil
	}

	ns, err := s.namespaceOrActive(ctx, app, req.Namespace)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, aurora.Sprintf(aurora.Red("failed to resolve namespace: %v"), err))
		sendExit(1)
		return nil
	}

	ops := optracker.New(stderr, stream)
	defer ops.AllDone() // Kill the tracker when we exit this function

	// Check for available update before we start the proc
	// so the output from the proc doesn't race with our
	// prints below.
	newVer := s.availableUpdate()

	// If force upgrade has been enabled, we force the upgrade now before we try and run the app
	if newVer != nil && newVer.ForceUpgrade {
		_, _ = fmt.Fprintf(stderr, aurora.Red("An urgent security update for Encore is available.").String()+"\n")
		if newVer.SecurityNotes != "" {
			_, _ = fmt.Fprintf(stderr, aurora.Sprintf(aurora.Yellow("%s"), newVer.SecurityNotes)+"\n")
		}

		_, _ = fmt.Fprintf(stderr, "Upgrading Encore to %v...\n", newVer.Version())
		if err := newVer.DoUpgrade(stderr, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, aurora.Sprintf(aurora.Red("Upgrade failed: %v"), err)+"\n")
		}

		slog.FlushBuffers()
		sendExit(1) // Kill the client
		os.Exit(1)  // Kill the daemon too
		return nil
	}

	// Hold the stream mutex so we can set up the stream map
	// before output starts.
	s.mu.Lock()

	// If the listen addr contains no interface, render it as "localhost:port"
	// instead of just ":port".
	displayListenAddr := req.ListenAddr
	if strings.HasPrefix(listenAddr, ":") {
		displayListenAddr = "localhost" + req.ListenAddr
	}

	browser := run.BrowserModeFromProto(req.Browser)
	if browser == run.BrowserModeAuto {
		browser = run.BrowserModeFromConfig(userConfig)
	}

	runInstance, err := s.mgr.Start(ctx, run.StartParams{
		App:        app,
		NS:         ns,
		WorkingDir: req.WorkingDir,
		Listener:   ln,
		ListenAddr: displayListenAddr,
		Watch:      req.Watch,
		Environ:    req.Environ,
		OpsTracker: ops,
		Browser:    browser,
		Debug:      run.DebugModeFromProto(req.DebugMode),
	})
	if err != nil {
		s.mu.Unlock()
		if errList := run.AsErrorList(err); errList != nil {
			_ = errList.SendToStream(stream)
		} else {
			errStr := err.Error()
			if !strings.HasSuffix(errStr, "\n") {
				errStr += "\n"
			}
			_, _ = stderr.Write([]byte(errStr))
		}
		sendExit(1)
		return nil
	}
	defer runInstance.Close()
	s.streams[runInstance.ID] = slog
	s.mu.Unlock()

	ops.AllDone()

	secrets, _ := s.sm.Load(app).Get(ctx, nil)
	externalDBs := map[string]string{}
	for key, val := range secrets.Values {
		if db, ok := strings.CutPrefix(key, "sqldb::"); ok {
			var connCfg struct {
				ConnString string `json:"connection_string"`
			}
			err := json.Unmarshal([]byte(val), &connCfg)
			if err != nil {
				log.Warn().Err(err).Str("key", key).Msg("failed to unmarshal connection string")
				continue
			}
			connURL, err := url.Parse(connCfg.ConnString)
			if err != nil {
				log.Warn().Err(err).Str("key", key).Msg("failed to parse connection string")
				continue
			}
			connURL.User = url.User(connURL.User.Username())
			externalDBs[db] = connURL.String()

		}
	}
	_, _ = stderr.Write([]byte("\n"))
	_, _ = fmt.Fprintf(stderr, "  Encore development server running!\n\n")

	_, _ = fmt.Fprintf(stderr, "  Your API is running at:     %s\n", aurora.Cyan("http://"+runInstance.ListenAddr))
	_, _ = fmt.Fprintf(stderr, "  Development Dashboard URL:  %s\n", aurora.Cyan(fmt.Sprintf(
		"%s/%s", s.mgr.DashBaseURL, app.PlatformOrLocalID())))
	_, _ = fmt.Fprintf(stderr, "  MCP SSE URL:                %s\n", aurora.Cyan(fmt.Sprintf(
		"%s/sse?appID=%s", s.mcp.BaseURL, app.PlatformOrLocalID())))

	if ns := runInstance.NS; !ns.Active || ns.Name != "default" {
		_, _ = fmt.Fprintf(stderr, "  Namespace:                  %s\n", aurora.Cyan(ns.Name))
		if len(externalDBs) > 0 {
			_, _ = fmt.Fprintln(stderr, "  External databases:")
		}
	}
	for db, connStr := range externalDBs {
		_, _ = fmt.Fprintf(stderr, "     %s: %s\n", db, aurora.Cyan(connStr))
	}
	if req.DebugMode == daemonpb.RunRequest_DEBUG_ENABLED {
		// Print the pid for debugging. Currently we only support this if we have a default gateway.
		if gw, ok := runInstance.ProcGroup().Gateways["api-gateway"]; ok {
			_, _ = fmt.Fprintf(stderr, "  Process ID:                 %d\n", aurora.Cyan(gw.Pid))
		}
	}
	// Log which experiments are enabled, if any
	if exp := runInstance.ProcGroup().Experiments.List(); len(exp) > 0 {
		strs := make([]string, len(exp))
		for i, e := range exp {
			strs[i] = string(e)
		}
		_, _ = fmt.Fprintf(stderr, "  Enabled experiment(s):      %s\n", aurora.Yellow(strings.Join(strs, ", ")))
	}

	// If there's a newer version available, print a message.
	if newVer != nil {
		if newVer.SecurityUpdate {
			_, _ = stderr.Write([]byte(aurora.Sprintf(
				aurora.Yellow("\n  New Encore release available with security updates: %s (you have %s)\n  Update with: encore version update\n"),
				newVer.Version(), version.Version)))

			if newVer.SecurityNotes != "" {
				_, _ = stderr.Write([]byte(aurora.Sprintf(
					aurora.Faint("\n  %s\n"),
					newVer.SecurityNotes)))
			}
		} else {
			_, _ = stderr.Write([]byte(aurora.Sprintf(
				aurora.Faint("\n  New Encore release available: %s (you have %s)\n  Update with: encore version update\n"),
				newVer.Version(), version.Version)))
		}
	}
	_, _ = stderr.Write([]byte("\n"))

	slog.FlushBuffers()

	go func() {
		// Wait a little bit for the app to start
		select {
		case <-runInstance.Done():
			return
		case <-time.After(5 * time.Second):
			if proc := runInstance.ProcGroup(); proc != nil {
				showFirstRunExperience(runInstance, proc.Meta, stderr)
			}
		}
	}()

	<-runInstance.Done() // wait for run to complete

	s.mu.Lock()
	delete(s.streams, runInstance.ID)
	s.mu.Unlock()
	return nil
}
