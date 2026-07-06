package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	backendplugin "github.com/steveyegge/beads/backend/plugin"
	"github.com/steveyegge/beads/internal/provider"
	"github.com/steveyegge/beads/internal/storage"
)

const protocolVersion = "gascity.backend.v1alpha1"

func main() {
	opts, args, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		usage()
		os.Exit(2)
	}
	if len(args) != 1 || args[0] != "serve" {
		usage()
		os.Exit(2)
	}
	if err := serve(context.Background(), os.Stdin, os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gc-backend-postgres [--trace=<path>|--trace-stderr] serve")
}

type options struct {
	tracePath   string
	traceStderr bool
}

func parseOptions(args []string) (options, []string, error) {
	var opts options
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--trace":
			if len(args) < 2 {
				return opts, args, fmt.Errorf("--trace requires a path")
			}
			opts.tracePath = args[1]
			args = args[2:]
		case strings.HasPrefix(arg, "--trace="):
			opts.tracePath = strings.TrimPrefix(arg, "--trace=")
			args = args[1:]
		case arg == "--trace-stderr":
			opts.traceStderr = true
			args = args[1:]
		case arg == "--no-trace":
			opts.tracePath = ""
			opts.traceStderr = false
			args = args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, args, fmt.Errorf("unknown option %q", arg)
		default:
			return opts, args, nil
		}
	}
	return opts, args, nil
}

func serve(ctx context.Context, in *os.File, out *os.File, opts options) error {
	manager := provider.NewManager()
	defer func() { _ = manager.CloseAll() }()

	tracer, err := newTracer(opts)
	if err != nil {
		return err
	}
	defer tracer.Close()

	enc := json.NewEncoder(out)
	if err := enc.Encode(response{OK: true, Result: mustJSON(map[string]string{"protocol": protocolVersion})}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var req request
		start := time.Now()
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			resp := errorResponse("", "bad_request", err)
			tracer.Log(traceEntryFromResponse(start, req, resp))
			_ = enc.Encode(resp)
			continue
		}
		resp := handle(ctx, manager, req)
		tracer.Log(traceEntryFromResponse(start, req, resp))
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type request struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     string          `json:"id,omitempty"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func handle(ctx context.Context, manager *provider.Manager, req request) response {
	switch req.Method {
	case "open":
		var p openParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := loadLocalEnv(p.BeadsDir); err != nil {
			return errorResponse(req.ID, "open_failed", err)
		}
		s, err := manager.Open(ctx, p.BeadsDir, p.Database, "main")
		if err != nil {
			return errorResponse(req.ID, "open_failed", err)
		}
		return ok(req.ID, map[string]string{"session_id": s.ID})
	case "close":
		var p sessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := manager.Close(p.SessionID); err != nil {
			return errorResponse(req.ID, "close_failed", err)
		}
		return ok(req.ID, map[string]bool{"closed": true})
	case "create_issue":
		var p createIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.CreateIssue(ctx, p.Issue, p.Actor, p.Commit, p.Message)
		return issueResponse(req.ID, "create_issue_failed", issue, err)
	case "get_issue":
		var p issueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.GetIssue(ctx, p.ID)
		return issueResponse(req.ID, "get_issue_failed", issue, err)
	case "update_issue":
		var p updateIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.UpdateIssue(ctx, p.ID, p.Updates, p.Actor, p.Commit, p.Message)
		return issueResponse(req.ID, "update_issue_failed", issue, err)
	case "close_issue":
		var p closeIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CloseIssue(ctx, p.ID, p.Reason, p.Actor, ""); err != nil {
			return storageErrorResponse(req.ID, "close_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "reopen_issue":
		var p reopenIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ReopenIssue(ctx, p.ID, p.Reason, p.Actor); err != nil {
			return storageErrorResponse(req.ID, "reopen_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "delete_issue":
		var p issueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.DeleteIssue(ctx, p.ID); err != nil {
			return storageErrorResponse(req.ID, "delete_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "search_issues":
		var p searchIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.SearchIssues(ctx, p.Query, p.Filter)
		if err != nil {
			return storageErrorResponse(req.ID, "search_issues_failed", err)
		}
		return ok(req.ID, issues)
	case "list_wisps":
		var p wispParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.ListWisps(ctx, p.Filter)
		if err != nil {
			return storageErrorResponse(req.ID, "list_wisps_failed", err)
		}
		return ok(req.ID, issues)
	case "ready_work":
		var p readyWorkParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.ReadyWork(ctx, p.Filter)
		if err != nil {
			return storageErrorResponse(req.ID, "ready_work_failed", err)
		}
		return ok(req.ID, issues)
	case "slot_set":
		var p slotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SlotSet(ctx, p.IssueID, p.Key, p.Value, p.Actor); err != nil {
			return storageErrorResponse(req.ID, "slot_set_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID, "key": p.Key})
	case "add_dependency":
		var p dependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.AddDependency(ctx, p.Dependency, p.Actor); err != nil {
			return storageErrorResponse(req.ID, "add_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"added": true})
	case "remove_dependency":
		var p dependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RemoveDependency(ctx, p.IssueID, p.DependsOnID, p.Actor); err != nil {
			return storageErrorResponse(req.ID, "remove_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"removed": true})
	case "get_dependencies":
		var p issueRefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetDependencies(ctx, p.issueID())
		if err != nil {
			return storageErrorResponse(req.ID, "get_dependencies_failed", err)
		}
		return ok(req.ID, issues)
	case "get_dependents":
		var p issueRefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetDependents(ctx, p.issueID())
		if err != nil {
			return storageErrorResponse(req.ID, "get_dependents_failed", err)
		}
		return ok(req.ID, issues)
	default:
		return errorResponse(req.ID, "unknown_method", fmt.Errorf("%s", req.Method))
	}
}

type openParams struct {
	BeadsDir string `json:"beads_dir"`
	Database string `json:"database,omitempty"`
}

type sessionParams struct {
	SessionID string `json:"session_id"`
}

type createIssueParams struct {
	SessionID string               `json:"session_id"`
	Issue     *backendplugin.Issue `json:"issue"`
	Actor     string               `json:"actor,omitempty"`
	Commit    bool                 `json:"commit,omitempty"`
	Message   string               `json:"message,omitempty"`
}

type issueIDParams struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
}

type issueRefParams struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id,omitempty"`
	IssueID   string `json:"issue_id,omitempty"`
}

func (p issueRefParams) issueID() string {
	if p.IssueID != "" {
		return p.IssueID
	}
	return p.ID
}

type updateIssueParams struct {
	SessionID string         `json:"session_id"`
	ID        string         `json:"id"`
	Updates   map[string]any `json:"updates"`
	Actor     string         `json:"actor,omitempty"`
	Commit    bool           `json:"commit,omitempty"`
	Message   string         `json:"message,omitempty"`
}

type closeIssueParams struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

type reopenIssueParams struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

type searchIssuesParams struct {
	SessionID string                    `json:"session_id"`
	Query     string                    `json:"query,omitempty"`
	Filter    backendplugin.IssueFilter `json:"filter,omitempty"`
}

type readyWorkParams struct {
	SessionID string                   `json:"session_id"`
	Filter    backendplugin.WorkFilter `json:"filter,omitempty"`
}

type wispParams struct {
	SessionID string                   `json:"session_id"`
	Filter    backendplugin.WispFilter `json:"filter,omitempty"`
}

type slotParams struct {
	SessionID string `json:"session_id"`
	IssueID   string `json:"issue_id"`
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

type dependencyParams struct {
	SessionID   string                    `json:"session_id"`
	Dependency  *backendplugin.Dependency `json:"dependency,omitempty"`
	IssueID     string                    `json:"issue_id,omitempty"`
	DependsOnID string                    `json:"depends_on_id,omitempty"`
	Actor       string                    `json:"actor,omitempty"`
}

func issueResponse(id, code string, issue *backendplugin.Issue, err error) response {
	if err != nil {
		return storageErrorResponse(id, code, err)
	}
	return ok(id, issue)
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, out)
}

func ok(id string, payload any) response {
	return response{ID: id, OK: true, Result: mustJSON(payload)}
}

func storageErrorResponse(id, code string, err error) response {
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		code = "not_found"
	}
	return errorResponse(id, code, err)
}

func errorResponse(id, code string, err error) response {
	return response{
		ID: id,
		OK: false,
		Error: &responseError{
			Code:    code,
			Message: err.Error(),
		},
	}
}

func mustJSON(payload any) json.RawMessage {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return data
}

func loadLocalEnv(beadsDir string) error {
	if strings.TrimSpace(beadsDir) == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(beadsDir, ".env"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load local env: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		if key != "BEADS_PG_PASSWORD" && key != "BEADS_PG_PASSWORD_COMMAND" {
			continue
		}
		if err := os.Setenv(key, parseEnvValue(strings.TrimSpace(value))); err != nil {
			return fmt.Errorf("set %s from local env: %w", key, err)
		}
	}
	return nil
}

func parseEnvValue(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "'\"'\"'", "'")
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var decoded string
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			return decoded
		}
	}
	return value
}

type tracer struct {
	out io.WriteCloser
	enc *json.Encoder
}

type traceEntry struct {
	Timestamp  string `json:"ts"`
	PID        int    `json:"pid"`
	Backend    string `json:"backend"`
	RequestID  string `json:"request_id,omitempty"`
	Method     string `json:"method,omitempty"`
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func newTracer(opts options) (*tracer, error) {
	if env := os.Getenv("GASCITY_BACKEND_POSTGRES_TRACE"); env != "" {
		switch strings.ToLower(env) {
		case "0", "false", "off", "none":
			opts.tracePath = ""
			opts.traceStderr = false
		case "stderr":
			opts.tracePath = ""
			opts.traceStderr = true
		default:
			opts.tracePath = env
			opts.traceStderr = false
		}
	}
	if opts.traceStderr {
		return &tracer{out: nopWriteCloser{os.Stderr}, enc: json.NewEncoder(os.Stderr)}, nil
	}
	if opts.tracePath == "" {
		return &tracer{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.tracePath), 0o755); err != nil {
		return nil, fmt.Errorf("create trace dir: %w", err)
	}
	f, err := os.OpenFile(opts.tracePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	return &tracer{out: f, enc: json.NewEncoder(f)}, nil
}

func (t *tracer) Log(entry traceEntry) {
	if t == nil || t.enc == nil {
		return
	}
	_ = t.enc.Encode(entry)
}

func (t *tracer) Close() {
	if t == nil || t.out == nil {
		return
	}
	_ = t.out.Close()
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func traceEntryFromResponse(start time.Time, req request, resp response) traceEntry {
	var code, message string
	if resp.Error != nil {
		code = resp.Error.Code
		message = sanitizeTraceText(resp.Error.Message, 500)
	}
	return traceEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		PID:        os.Getpid(),
		Backend:    "postgres-gascity",
		RequestID:  req.ID,
		Method:     req.Method,
		OK:         resp.OK,
		ErrorCode:  code,
		Error:      message,
		DurationMS: time.Since(start).Milliseconds(),
	}
}

func sanitizeTraceText(s string, limit int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(strings.TrimSpace(s))
	if limit > 0 && len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
