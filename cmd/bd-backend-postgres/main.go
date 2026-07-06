package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	backendplugin "github.com/steveyegge/beads/backend/plugin"
	"github.com/steveyegge/beads/internal/provider"
)

func main() {
	opts, args, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		usage()
		os.Exit(2)
	}
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}

	var payload any
	switch args[0] {
	case "capabilities":
		payload = provider.BackendCapabilities()
	case "doctor":
		payload = provider.Doctor()
	case "serve":
		if err := serve(context.Background(), os.Stdin, os.Stdout, opts); err != nil {
			fmt.Fprintf(os.Stderr, "serve: %v\n", err)
			os.Exit(1)
		}
		return
	default:
		usage()
		os.Exit(2)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(os.Stderr, "encode response: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bd-backend-postgres [--trace=<path>|--trace-stderr] <capabilities|doctor|serve>")
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
	hello := backendplugin.Hello{
		Protocol:     backendplugin.ProtocolVersion,
		Backend:      provider.Name,
		Capabilities: provider.BackendCapabilities(),
	}
	if err := enc.Encode(backendplugin.Response{OK: true, Result: mustJSON(hello)}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var req backendplugin.Request
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

	RetryCount      *int   `json:"retry_count,omitempty"`
	RetryErrorClass string `json:"retry_error_class,omitempty"`
	RetryError      string `json:"retry_error,omitempty"`
	LockWaitCount   *int   `json:"lock_wait_count,omitempty"`
	LockWaitMS      *int64 `json:"lock_wait_ms,omitempty"`
	MaxLockWaitMS   *int64 `json:"max_lock_wait_ms,omitempty"`
}

func newTracer(opts options) (*tracer, error) {
	if env := os.Getenv("BEADS_BACKEND_POSTGRES_TRACE"); env != "" {
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

func traceEntryFromResponse(start time.Time, req backendplugin.Request, resp backendplugin.Response) traceEntry {
	var code, message string
	if resp.Error != nil {
		code = resp.Error.Code
		message = sanitizeTraceText(resp.Error.Message, 500)
	}
	entry := traceEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		PID:        os.Getpid(),
		Backend:    provider.Name,
		RequestID:  req.ID,
		Method:     req.Method,
		OK:         resp.OK,
		ErrorCode:  code,
		Error:      message,
		DurationMS: time.Since(start).Milliseconds(),
	}
	return entry
}

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

func sanitizeTraceText(s string, limit int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(strings.TrimSpace(s))
	if limit > 0 && len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

func handle(ctx context.Context, manager *provider.Manager, req backendplugin.Request) backendplugin.Response {
	switch req.Method {
	case "capabilities":
		return ok(req.ID, provider.BackendCapabilities())
	case "doctor":
		return ok(req.ID, provider.Doctor())
	case "init":
		var p backendplugin.InitParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Init(ctx, p.BeadsDir, p.Database, p.Branch, p.Prefix, p.Actor)
		if err != nil {
			return errorResponse(req.ID, "init_failed", err)
		}
		return ok(req.ID, backendplugin.OpenResult{SessionID: s.ID})
	case "open":
		var p backendplugin.OpenParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Open(ctx, p.BeadsDir, p.Database, p.Branch)
		if err != nil {
			return errorResponse(req.ID, "open_failed", err)
		}
		return ok(req.ID, backendplugin.OpenResult{SessionID: s.ID})
	case "close":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := manager.Close(p.SessionID); err != nil {
			return errorResponse(req.ID, "close_failed", err)
		}
		return ok(req.ID, map[string]bool{"closed": true})
	case "begin_transaction":
		var p backendplugin.TransactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		txID, err := manager.BeginTransaction(ctx, p.SessionID, p.CommitMsg)
		if err != nil {
			return errorResponse(req.ID, "begin_transaction_failed", err)
		}
		return ok(req.ID, map[string]string{"tx_id": txID})
	case "commit_transaction":
		var p backendplugin.TransactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := manager.CommitTransaction(ctx, p.TxID); err != nil {
			return errorResponse(req.ID, "commit_transaction_failed", err)
		}
		return ok(req.ID, map[string]bool{"committed": true})
	case "rollback_transaction":
		var p backendplugin.TransactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := manager.RollbackTransaction(ctx, p.TxID); err != nil {
			return errorResponse(req.ID, "rollback_transaction_failed", err)
		}
		return ok(req.ID, map[string]bool{"rolled_back": true})
	case "path":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		return ok(req.ID, map[string]string{"path": s.Path()})
	case "cli_dir":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		return ok(req.ID, map[string]string{"path": s.CLIDir()})
	case "set_config":
		var p backendplugin.ConfigParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SetConfig(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "set_config_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "get_config":
		var p backendplugin.ConfigParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		value, err := s.GetConfig(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "get_config_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "get_all_config":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		values, err := s.GetAllConfig(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_all_config_failed", err)
		}
		return ok(req.ID, values)
	case "raw_sql":
		var p backendplugin.RawSQLParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		result, err := s.ExecuteRawSQL(ctx, p.Query)
		if err != nil {
			return errorResponse(req.ID, "raw_sql_failed", err)
		}
		return ok(req.ID, result)
	case "delete_config":
		var p backendplugin.ConfigParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.DeleteConfig(ctx, p.Key); err != nil {
			return errorResponse(req.ID, "delete_config_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "get_custom_statuses":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		values, err := s.GetCustomStatuses(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_custom_statuses_failed", err)
		}
		return ok(req.ID, values)
	case "get_custom_statuses_detailed":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		values, err := s.GetCustomStatusesDetailed(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_custom_statuses_detailed_failed", err)
		}
		return ok(req.ID, values)
	case "get_custom_types":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		values, err := s.GetCustomTypes(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_custom_types_failed", err)
		}
		return ok(req.ID, values)
	case "get_infra_types":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		return ok(req.ID, s.GetInfraTypes(ctx))
	case "is_infra_type":
		var p backendplugin.IssueTypeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		return ok(req.ID, map[string]bool{"ok": s.IsInfraTypeCtx(ctx, p.IssueType)})
	case "set_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SetMetadata(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "set_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "get_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		value, err := s.GetMetadata(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "get_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "set_local_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SetLocalMetadata(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "set_local_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "get_local_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		value, err := s.GetLocalMetadata(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "get_local_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "create_issue":
		var p backendplugin.CreateIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.CreateIssue(ctx, p.Issue, p.Actor, p.Commit, p.Message)
		if err != nil {
			return errorResponse(req.ID, "create_issue_failed", err)
		}
		return ok(req.ID, issue)
	case "create_issues":
		var p backendplugin.CreateIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CreateIssues(ctx, p.Issues, p.Actor); err != nil {
			return errorResponse(req.ID, "create_issues_failed", err)
		}
		return ok(req.ID, p.Issues)
	case "create_issues_with_full_options":
		var p backendplugin.CreateIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CreateIssuesWithFullOptions(ctx, p.Issues, p.Actor, p.Options); err != nil {
			return errorResponse(req.ID, "create_issues_with_full_options_failed", err)
		}
		return ok(req.ID, p.Issues)
	case "get_issue":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.GetIssue(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_issue_failed", err)
		}
		return ok(req.ID, issue)
	case "get_issue_by_external_ref":
		var p backendplugin.ExternalRefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.GetIssueByExternalRef(ctx, p.ExternalRef)
		if err != nil {
			return errorResponse(req.ID, "get_issue_by_external_ref_failed", err)
		}
		return ok(req.ID, issue)
	case "get_issues_by_ids":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetIssuesByIDs(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_issues_by_ids_failed", err)
		}
		return ok(req.ID, issues)
	case "search_issues":
		var p backendplugin.SearchIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.SearchIssues(ctx, p.Query, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "search_issues_failed", err)
		}
		return ok(req.ID, issues)
	case "search_issues_with_counts":
		var p backendplugin.SearchIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.SearchIssuesWithCounts(ctx, p.Query, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "search_issues_with_counts_failed", err)
		}
		return ok(req.ID, issues)
	case "update_issue":
		var p backendplugin.UpdateIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.UpdateIssue(ctx, p.ID, p.Updates, p.Actor, p.Commit, p.Message)
		if err != nil {
			return errorResponse(req.ID, "update_issue_failed", err)
		}
		return ok(req.ID, issue)
	case "reopen_issue":
		var p backendplugin.ReopenIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ReopenIssue(ctx, p.ID, p.Reason, p.Actor); err != nil {
			return errorResponse(req.ID, "reopen_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "update_issue_type":
		var p backendplugin.UpdateIssueTypeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.UpdateIssueType(ctx, p.ID, p.IssueType, p.Actor); err != nil {
			return errorResponse(req.ID, "update_issue_type_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "close_issue":
		var p backendplugin.CloseIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CloseIssue(ctx, p.ID, p.Reason, p.Actor, p.Session); err != nil {
			return errorResponse(req.ID, "close_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "delete_issue":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.DeleteIssue(ctx, p.ID); err != nil {
			return errorResponse(req.ID, "delete_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "delete_issues":
		var p backendplugin.DeleteIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		result, err := s.DeleteIssues(ctx, p.IDs, p.Cascade, p.Force, p.DryRun)
		if err != nil {
			return errorResponse(req.ID, "delete_issues_failed", err)
		}
		return ok(req.ID, result)
	case "delete_issues_by_source_repo":
		var p backendplugin.SourceRepoParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.DeleteIssuesBySourceRepo(ctx, p.SourceRepo)
		if err != nil {
			return errorResponse(req.ID, "delete_issues_by_source_repo_failed", err)
		}
		return ok(req.ID, map[string]int{"count": count})
	case "update_issue_id":
		var p backendplugin.UpdateIssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.UpdateIssueID(ctx, p.OldID, p.NewID, p.Issue, p.Actor); err != nil {
			return errorResponse(req.ID, "update_issue_id_failed", err)
		}
		return ok(req.ID, map[string]string{"old_id": p.OldID, "new_id": p.NewID})
	case "claim_issue":
		var p backendplugin.ClaimIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ClaimIssue(ctx, p.ID, p.Actor); err != nil {
			return errorResponse(req.ID, "claim_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "claim_ready_issue":
		var p backendplugin.ClaimIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.ClaimReadyIssue(ctx, p.Filter, p.Actor)
		if err != nil {
			return errorResponse(req.ID, "claim_ready_issue_failed", err)
		}
		return ok(req.ID, issue)
	case "heartbeat_issue":
		var p backendplugin.ClaimIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.HeartbeatIssue(ctx, p.ID, p.Actor); err != nil {
			return errorResponse(req.ID, "heartbeat_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "reclaim_expired_leases":
		var p backendplugin.ReclaimExpiredLeasesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		reclaimed, err := s.ReclaimExpiredLeases(ctx, p.OlderThan, p.Actor)
		if err != nil {
			return errorResponse(req.ID, "reclaim_expired_leases_failed", err)
		}
		return ok(req.ID, reclaimed)
	case "promote_from_ephemeral":
		var p backendplugin.ClaimIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.PromoteFromEphemeral(ctx, p.ID, p.Actor); err != nil {
			return errorResponse(req.ID, "promote_from_ephemeral_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "get_next_child_id":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		id, err := s.GetNextChildID(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_next_child_id_failed", err)
		}
		return ok(req.ID, map[string]string{"id": id})
	case "rename_counter_prefix":
		var p backendplugin.PrefixRenameParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RenameCounterPrefix(ctx, p.OldPrefix, p.NewPrefix); err != nil {
			return errorResponse(req.ID, "rename_counter_prefix_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "rename_dependency_prefix":
		var p backendplugin.PrefixRenameParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RenameDependencyPrefix(ctx, p.OldPrefix, p.NewPrefix); err != nil {
			return errorResponse(req.ID, "rename_dependency_prefix_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "add_dependency":
		var p backendplugin.DependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.AddDependency(ctx, p.Dependency, p.Actor); err != nil {
			return errorResponse(req.ID, "add_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"added": true})
	case "remove_dependency":
		var p backendplugin.DependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RemoveDependency(ctx, p.IssueID, p.DependsOnID, p.Actor); err != nil {
			return errorResponse(req.ID, "remove_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"removed": true})
	case "get_dependencies":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetDependencies(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_dependencies_failed", err)
		}
		return ok(req.ID, issues)
	case "get_dependents":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetDependents(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_dependents_failed", err)
		}
		return ok(req.ID, issues)
	case "get_dependency_tree":
		var p backendplugin.DependencyTreeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		tree, err := s.GetDependencyTree(ctx, p.IssueID, p.MaxDepth, p.ShowAllPaths, p.Reverse)
		if err != nil {
			return errorResponse(req.ID, "get_dependency_tree_failed", err)
		}
		return ok(req.ID, tree)
	case "add_label":
		var p backendplugin.AddLabelParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		labels, err := s.AddLabel(ctx, p.ID, p.Label, p.Actor, p.Commit, p.Message)
		if err != nil {
			return errorResponse(req.ID, "add_label_failed", err)
		}
		return ok(req.ID, labels)
	case "remove_label":
		var p backendplugin.LabelParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RemoveLabel(ctx, p.ID, p.Label, p.Actor); err != nil {
			return errorResponse(req.ID, "remove_label_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID, "label": p.Label})
	case "get_labels":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		labels, err := s.GetLabels(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_labels_failed", err)
		}
		return ok(req.ID, labels)
	case "get_issues_by_label":
		var p backendplugin.LabelParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetIssuesByLabel(ctx, p.Label)
		if err != nil {
			return errorResponse(req.ID, "get_issues_by_label_failed", err)
		}
		return ok(req.ID, issues)
	case "get_dependencies_with_metadata":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		deps, err := s.GetDependenciesWithMetadata(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_dependencies_with_metadata_failed", err)
		}
		return ok(req.ID, deps)
	case "get_dependents_with_metadata":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		deps, err := s.GetDependentsWithMetadata(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_dependents_with_metadata_failed", err)
		}
		return ok(req.ID, deps)
	case "get_dependency_records":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		deps, err := s.GetDependencyRecords(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_dependency_records_failed", err)
		}
		return ok(req.ID, deps)
	case "get_dependency_records_for_issues":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		deps, err := s.GetDependencyRecordsForIssues(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_dependency_records_for_issues_failed", err)
		}
		return ok(req.ID, deps)
	case "get_all_dependency_records":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		deps, err := s.GetAllDependencyRecords(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_all_dependency_records_failed", err)
		}
		return ok(req.ID, deps)
	case "get_dependency_counts":
		var p backendplugin.DependencyCountsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		counts, err := s.GetDependencyCounts(ctx, p.IssueIDs)
		if err != nil {
			return errorResponse(req.ID, "get_dependency_counts_failed", err)
		}
		return ok(req.ID, counts)
	case "get_blocking_info_for_issues":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		blockedBy, blocks, parents, err := s.GetBlockingInfoForIssues(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_blocking_info_for_issues_failed", err)
		}
		return ok(req.ID, backendplugin.BlockingInfoResult{BlockedBy: blockedBy, Blocks: blocks, Parents: parents})
	case "is_blocked":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		blocked, blockedBy, err := s.IsBlocked(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "is_blocked_failed", err)
		}
		return ok(req.ID, backendplugin.IsBlockedResult{Blocked: blocked, BlockedBy: blockedBy})
	case "get_newly_unblocked_by_close":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetNewlyUnblockedByClose(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_newly_unblocked_by_close_failed", err)
		}
		return ok(req.ID, issues)
	case "detect_cycles":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		cycles, err := s.DetectCycles(ctx)
		if err != nil {
			return errorResponse(req.ID, "detect_cycles_failed", err)
		}
		return ok(req.ID, cycles)
	case "find_wisp_dependents_recursive":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		found, err := s.FindWispDependentsRecursive(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "find_wisp_dependents_recursive_failed", err)
		}
		return ok(req.ID, found)
	case "count_dependents_by_status":
		var p backendplugin.StatusCountParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountDependentsByStatus(ctx, p.IssueID, p.Status)
		if err != nil {
			return errorResponse(req.ID, "count_dependents_by_status_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "get_issue_comments":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		comments, err := s.GetIssueComments(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "get_issue_comments_failed", err)
		}
		return ok(req.ID, comments)
	case "add_issue_comment":
		var p backendplugin.CommentParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		comment, err := s.AddIssueComment(ctx, p.IssueID, p.Author, p.Text)
		if err != nil {
			return errorResponse(req.ID, "add_issue_comment_failed", err)
		}
		return ok(req.ID, comment)
	case "add_comment":
		var p backendplugin.CommentParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.AddComment(ctx, p.IssueID, p.Author, p.Text); err != nil {
			return errorResponse(req.ID, "add_comment_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID})
	case "import_issue_comment":
		var p backendplugin.CommentParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		comment, err := s.ImportIssueComment(ctx, p.IssueID, p.Author, p.Text, p.CreatedAt)
		if err != nil {
			return errorResponse(req.ID, "import_issue_comment_failed", err)
		}
		return ok(req.ID, comment)
	case "get_events":
		var p backendplugin.EventsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		events, err := s.GetEvents(ctx, p.IssueID, p.Limit)
		if err != nil {
			return errorResponse(req.ID, "get_events_failed", err)
		}
		return ok(req.ID, events)
	case "get_all_events_since":
		var p backendplugin.EventsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		events, err := s.GetAllEventsSince(ctx, p.Since)
		if err != nil {
			return errorResponse(req.ID, "get_all_events_since_failed", err)
		}
		return ok(req.ID, events)
	case "get_comment_counts":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		counts, err := s.GetCommentCounts(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_comment_counts_failed", err)
		}
		return ok(req.ID, counts)
	case "get_comments_for_issues":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		comments, err := s.GetCommentsForIssues(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_comments_for_issues_failed", err)
		}
		return ok(req.ID, comments)
	case "get_labels_for_issues":
		var p backendplugin.IssueIDsParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		labels, err := s.GetLabelsForIssues(ctx, p.IDs)
		if err != nil {
			return errorResponse(req.ID, "get_labels_for_issues_failed", err)
		}
		return ok(req.ID, labels)
	case "ready_work":
		var p struct {
			SessionID string                   `json:"session_id"`
			Filter    backendplugin.WorkFilter `json:"filter,omitempty"`
		}
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.ReadyWork(ctx, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "ready_work_failed", err)
		}
		return ok(req.ID, issues)
	case "ready_work_with_counts":
		var p backendplugin.ReadyWorkParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.ReadyWorkWithCounts(ctx, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "ready_work_with_counts_failed", err)
		}
		return ok(req.ID, issues)
	case "blocked_issues":
		var p backendplugin.ReadyWorkParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.BlockedIssues(ctx, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "blocked_issues_failed", err)
		}
		return ok(req.ID, issues)
	case "epics_eligible_for_closure":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		epics, err := s.EpicsEligibleForClosure(ctx)
		if err != nil {
			return errorResponse(req.ID, "epics_eligible_for_closure_failed", err)
		}
		return ok(req.ID, epics)
	case "list_wisps":
		var p backendplugin.WispParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		wisps, err := s.ListWisps(ctx, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "list_wisps_failed", err)
		}
		return ok(req.ID, wisps)
	case "count_issues":
		var p backendplugin.CountIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountIssues(ctx, p.Query, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "count_issues_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "count_issues_by_group":
		var p backendplugin.CountIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		counts, err := s.CountIssuesByGroup(ctx, p.Filter, p.GroupBy)
		if err != nil {
			return errorResponse(req.ID, "count_issues_by_group_failed", err)
		}
		return ok(req.ID, counts)
	case "count_dependents":
		var p backendplugin.CountIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountDependents(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "count_dependents_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "count_dependencies":
		var p backendplugin.CountIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountDependencies(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "count_dependencies_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "count_issue_comments":
		var p backendplugin.CountIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountIssueComments(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "count_issue_comments_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "count_events":
		var p backendplugin.CountIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		count, err := s.CountEvents(ctx, p.IssueID, p.Limit)
		if err != nil {
			return errorResponse(req.ID, "count_events_failed", err)
		}
		return ok(req.ID, map[string]int64{"count": count})
	case "statistics":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		stats, err := s.Statistics(ctx)
		if err != nil {
			return errorResponse(req.ID, "statistics_failed", err)
		}
		return ok(req.ID, stats)
	case "get_repo_mtime":
		var p backendplugin.RepoMtimeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		mtime, err := s.GetRepoMtime(ctx, p.RepoPath)
		if err != nil {
			return errorResponse(req.ID, "get_repo_mtime_failed", err)
		}
		return ok(req.ID, map[string]int64{"mtime_ns": mtime})
	case "set_repo_mtime":
		var p backendplugin.RepoMtimeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SetRepoMtime(ctx, p.RepoPath, p.JSONLPath, p.MtimeNS); err != nil {
			return errorResponse(req.ID, "set_repo_mtime_failed", err)
		}
		return ok(req.ID, map[string]string{"repo_path": p.RepoPath})
	case "clear_repo_mtime":
		var p backendplugin.RepoMtimeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ClearRepoMtime(ctx, p.RepoPath); err != nil {
			return errorResponse(req.ID, "clear_repo_mtime_failed", err)
		}
		return ok(req.ID, map[string]string{"repo_path": p.RepoPath})
	case "get_molecule_progress":
		var p backendplugin.MoleculeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		progress, err := s.GetMoleculeProgress(ctx, p.MoleculeID)
		if err != nil {
			return errorResponse(req.ID, "get_molecule_progress_failed", err)
		}
		return ok(req.ID, progress)
	case "get_molecule_last_activity":
		var p backendplugin.MoleculeParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		activity, err := s.GetMoleculeLastActivity(ctx, p.MoleculeID)
		if err != nil {
			return errorResponse(req.ID, "get_molecule_last_activity_failed", err)
		}
		return ok(req.ID, activity)
	case "get_stale_issues":
		var p backendplugin.StaleIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issues, err := s.GetStaleIssues(ctx, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "get_stale_issues_failed", err)
		}
		return ok(req.ID, issues)
	case "dolt_gc":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.DoltGC(ctx); err != nil {
			return errorResponse(req.ID, "dolt_gc_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "flatten":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Flatten(ctx); err != nil {
			return errorResponse(req.ID, "flatten_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "compact":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Compact(ctx, p.InitialHash, p.BoundaryHash, p.OldCommits, p.RecentHashes); err != nil {
			return errorResponse(req.ID, "compact_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "check_eligibility":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		eligible, reason, err := s.CheckEligibility(ctx, p.IssueID, p.Tier)
		if err != nil {
			return errorResponse(req.ID, "check_eligibility_failed", err)
		}
		return ok(req.ID, backendplugin.EligibilityResult{Eligible: eligible, Reason: reason})
	case "apply_compaction":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ApplyCompaction(ctx, p.IssueID, p.Tier, p.OriginalSize, p.CompactedSize, p.CommitHash); err != nil {
			return errorResponse(req.ID, "apply_compaction_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID})
	case "snapshot_issue":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SnapshotIssue(ctx, p.IssueID, p.Tier); err != nil {
			return errorResponse(req.ID, "snapshot_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID})
	case "get_compaction_snapshot":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		snapshot, err := s.GetCompactionSnapshot(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "get_compaction_snapshot_failed", err)
		}
		return ok(req.ID, snapshot)
	case "restore_from_snapshot":
		var p backendplugin.CompactionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		snapshot, err := s.RestoreFromSnapshot(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "restore_from_snapshot_failed", err)
		}
		return ok(req.ID, snapshot)
	case "get_tier1_candidates":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		candidates, err := s.GetTier1Candidates(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_tier1_candidates_failed", err)
		}
		return ok(req.ID, candidates)
	case "get_tier2_candidates":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		candidates, err := s.GetTier2Candidates(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_tier2_candidates_failed", err)
		}
		return ok(req.ID, candidates)
	case "merge_slot_create":
		var p backendplugin.MergeSlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.MergeSlotCreate(ctx, p.Actor)
		if err != nil {
			return errorResponse(req.ID, "merge_slot_create_failed", err)
		}
		return ok(req.ID, issue)
	case "merge_slot_check":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		status, err := s.MergeSlotCheck(ctx)
		if err != nil {
			return errorResponse(req.ID, "merge_slot_check_failed", err)
		}
		return ok(req.ID, status)
	case "merge_slot_acquire":
		var p backendplugin.MergeSlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		result, err := s.MergeSlotAcquire(ctx, p.Holder, p.Actor, p.Wait)
		if err != nil {
			return errorResponse(req.ID, "merge_slot_acquire_failed", err)
		}
		return ok(req.ID, result)
	case "merge_slot_release":
		var p backendplugin.MergeSlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.MergeSlotRelease(ctx, p.Holder, p.Actor); err != nil {
			return errorResponse(req.ID, "merge_slot_release_failed", err)
		}
		return ok(req.ID, map[string]bool{"released": true})
	case "slot_set":
		var p backendplugin.SlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SlotSet(ctx, p.IssueID, p.Key, p.Value, p.Actor); err != nil {
			return errorResponse(req.ID, "slot_set_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID, "key": p.Key})
	case "slot_get":
		var p backendplugin.SlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		value, err := s.SlotGet(ctx, p.IssueID, p.Key)
		if err != nil {
			return errorResponse(req.ID, "slot_get_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID, "key": p.Key, "value": value})
	case "slot_clear":
		var p backendplugin.SlotParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.SlotClear(ctx, p.IssueID, p.Key, p.Actor); err != nil {
			return errorResponse(req.ID, "slot_clear_failed", err)
		}
		return ok(req.ID, map[string]string{"issue_id": p.IssueID, "key": p.Key})
	case "commit_merge_resolution":
		var p backendplugin.CommitParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CommitMergeResolution(ctx, p.Message); err != nil {
			return errorResponse(req.ID, "commit_merge_resolution_failed", err)
		}
		return ok(req.ID, map[string]bool{"committed": true})
	case "branch":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.CreateBranch(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "branch_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "checkout":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Checkout(ctx, p.Branch); err != nil {
			return errorResponse(req.ID, "checkout_failed", err)
		}
		return ok(req.ID, map[string]string{"branch": p.Branch})
	case "current_branch":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		branch, err := s.CurrentBranch(ctx)
		if err != nil {
			return errorResponse(req.ID, "current_branch_failed", err)
		}
		return ok(req.ID, map[string]string{"branch": branch})
	case "delete_branch":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.DeleteBranch(ctx, p.Branch); err != nil {
			return errorResponse(req.ID, "delete_branch_failed", err)
		}
		return ok(req.ID, map[string]string{"branch": p.Branch})
	case "list_branches":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		branches, err := s.ListBranches(ctx)
		if err != nil {
			return errorResponse(req.ID, "list_branches_failed", err)
		}
		return ok(req.ID, branches)
	case "commit_exists":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		okay, err := s.CommitExists(ctx, p.Hash)
		if err != nil {
			return errorResponse(req.ID, "commit_exists_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": okay})
	case "get_current_commit":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		hash, err := s.GetCurrentCommit(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_current_commit_failed", err)
		}
		return ok(req.ID, map[string]string{"hash": hash})
	case "status":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		status, err := s.Status(ctx)
		if err != nil {
			return errorResponse(req.ID, "status_failed", err)
		}
		return ok(req.ID, status)
	case "log":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		entries, err := s.Log(ctx, p.Limit)
		if err != nil {
			return errorResponse(req.ID, "log_failed", err)
		}
		return ok(req.ID, entries)
	case "merge":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		conflicts, err := s.Merge(ctx, p.Branch)
		if err != nil {
			return errorResponse(req.ID, "merge_failed", err)
		}
		return ok(req.ID, conflicts)
	case "get_conflicts":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		conflicts, err := s.GetConflicts(ctx)
		if err != nil {
			return errorResponse(req.ID, "get_conflicts_failed", err)
		}
		return ok(req.ID, conflicts)
	case "resolve_conflicts":
		var p backendplugin.ResolveConflictParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ResolveConflicts(ctx, p.Table, p.Strategy); err != nil {
			return errorResponse(req.ID, "resolve_conflicts_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "history":
		var p backendplugin.HistoryParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		history, err := s.History(ctx, p.IssueID)
		if err != nil {
			return errorResponse(req.ID, "history_failed", err)
		}
		return ok(req.ID, history)
	case "as_of":
		var p backendplugin.HistoryParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		issue, err := s.AsOf(ctx, p.IssueID, p.Ref)
		if err != nil {
			return errorResponse(req.ID, "as_of_failed", err)
		}
		return ok(req.ID, issue)
	case "diff":
		var p backendplugin.HistoryParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		diff, err := s.Diff(ctx, p.FromRef, p.ToRef)
		if err != nil {
			return errorResponse(req.ID, "diff_failed", err)
		}
		return ok(req.ID, diff)
	case "add_remote":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.AddRemote(ctx, p.Name, p.URL); err != nil {
			return errorResponse(req.ID, "add_remote_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "remove_remote":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RemoveRemote(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "remove_remote_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "has_remote":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		okay, err := s.HasRemote(ctx, p.Name)
		if err != nil {
			return errorResponse(req.ID, "has_remote_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": okay})
	case "list_remotes":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		remotes, err := s.ListRemotes(ctx)
		if err != nil {
			return errorResponse(req.ID, "list_remotes_failed", err)
		}
		return ok(req.ID, remotes)
	case "push":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Push(ctx); err != nil {
			return errorResponse(req.ID, "push_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "pull":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Pull(ctx); err != nil {
			return errorResponse(req.ID, "pull_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "force_push":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.ForcePush(ctx); err != nil {
			return errorResponse(req.ID, "force_push_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "push_remote":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.PushRemote(ctx, p.Name, p.Force); err != nil {
			return errorResponse(req.ID, "push_remote_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "pull_remote":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.PullRemote(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "pull_remote_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "fetch":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Fetch(ctx, p.Peer); err != nil {
			return errorResponse(req.ID, "fetch_failed", err)
		}
		return ok(req.ID, map[string]string{"peer": p.Peer})
	case "push_to":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.PushTo(ctx, p.Peer); err != nil {
			return errorResponse(req.ID, "push_to_failed", err)
		}
		return ok(req.ID, map[string]string{"peer": p.Peer})
	case "pull_from":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		conflicts, err := s.PullFrom(ctx, p.Peer)
		if err != nil {
			return errorResponse(req.ID, "pull_from_failed", err)
		}
		return ok(req.ID, conflicts)
	case "backup_add":
		var p backendplugin.BackupParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.BackupAdd(ctx, p.Name, p.URL); err != nil {
			return errorResponse(req.ID, "backup_add_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "backup_sync":
		var p backendplugin.BackupParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.BackupSync(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "backup_sync_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "backup_remove":
		var p backendplugin.BackupParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.BackupRemove(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "backup_remove_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "backup_database":
		var p backendplugin.BackupParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.BackupDatabase(ctx, p.Dir); err != nil {
			return errorResponse(req.ID, "backup_database_failed", err)
		}
		return ok(req.ID, map[string]string{"dir": p.Dir})
	case "restore_database":
		var p backendplugin.BackupParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RestoreDatabase(ctx, p.Dir, p.Force); err != nil {
			return errorResponse(req.ID, "restore_database_failed", err)
		}
		return ok(req.ID, map[string]string{"dir": p.Dir})
	case "add_federation_peer":
		var p backendplugin.FederationPeerParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.AddFederationPeer(ctx, p.Peer); err != nil {
			return errorResponse(req.ID, "add_federation_peer_failed", err)
		}
		return ok(req.ID, map[string]bool{"ok": true})
	case "get_federation_peer":
		var p backendplugin.FederationPeerParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		peer, err := s.GetFederationPeer(ctx, p.Name)
		if err != nil {
			return errorResponse(req.ID, "get_federation_peer_failed", err)
		}
		return ok(req.ID, peer)
	case "list_federation_peers":
		var p backendplugin.SessionParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		peers, err := s.ListFederationPeers(ctx)
		if err != nil {
			return errorResponse(req.ID, "list_federation_peers_failed", err)
		}
		return ok(req.ID, peers)
	case "remove_federation_peer":
		var p backendplugin.FederationPeerParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.RemoveFederationPeer(ctx, p.Name); err != nil {
			return errorResponse(req.ID, "remove_federation_peer_failed", err)
		}
		return ok(req.ID, map[string]string{"name": p.Name})
	case "sync_status":
		var p backendplugin.RefParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		status, err := s.SyncStatus(ctx, p.Peer)
		if err != nil {
			return errorResponse(req.ID, "sync_status_failed", err)
		}
		return ok(req.ID, status)
	case "commit":
		var p backendplugin.CommitParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		s, err := manager.Get(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_session", err)
		}
		if err := s.Commit(ctx, p.Message); err != nil {
			return errorResponse(req.ID, "commit_failed", err)
		}
		return ok(req.ID, map[string]bool{"committed": true})
	case "tx_create_issue":
		var p backendplugin.CreateIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.CreateIssue(ctx, p.Issue, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_create_issue_failed", err)
		}
		return ok(req.ID, p.Issue)
	case "tx_create_issues":
		var p backendplugin.CreateIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.CreateIssues(ctx, p.Issues, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_create_issues_failed", err)
		}
		return ok(req.ID, p.Issues)
	case "tx_update_issue":
		var p backendplugin.UpdateIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := provider.NormalizeUpdatePayload(p.Updates); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		if err := tx.UpdateIssue(ctx, p.ID, p.Updates, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_update_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "tx_close_issue":
		var p backendplugin.CloseIssueParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.CloseIssue(ctx, p.ID, p.Reason, p.Actor, p.Session); err != nil {
			return errorResponse(req.ID, "tx_close_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "tx_delete_issue":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.DeleteIssue(ctx, p.ID); err != nil {
			return errorResponse(req.ID, "tx_delete_issue_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID})
	case "tx_get_issue":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		issue, err := tx.GetIssue(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "tx_get_issue_failed", err)
		}
		return ok(req.ID, issue)
	case "tx_search_issues":
		var p backendplugin.SearchIssuesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		issues, err := tx.SearchIssues(ctx, p.Query, p.Filter)
		if err != nil {
			return errorResponse(req.ID, "tx_search_issues_failed", err)
		}
		return ok(req.ID, issues)
	case "tx_add_dependency":
		var p backendplugin.DependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.AddDependencyWithOptions(ctx, p.Dependency, p.Actor, p.Options); err != nil {
			return errorResponse(req.ID, "tx_add_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"added": true})
	case "tx_remove_dependency":
		var p backendplugin.DependencyParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.RemoveDependency(ctx, p.IssueID, p.DependsOnID, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_remove_dependency_failed", err)
		}
		return ok(req.ID, map[string]bool{"removed": true})
	case "tx_get_dependency_records":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		deps, err := tx.GetDependencyRecords(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "tx_get_dependency_records_failed", err)
		}
		return ok(req.ID, deps)
	case "tx_cycle_through_edges":
		var p backendplugin.CycleEdgesParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		cycle, err := tx.CycleThroughEdges(ctx, p.Edges)
		if err != nil {
			return errorResponse(req.ID, "tx_cycle_through_edges_failed", err)
		}
		return ok(req.ID, map[string]string{"cycle": cycle})
	case "tx_add_label":
		var p backendplugin.AddLabelParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.AddLabel(ctx, p.ID, p.Label, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_add_label_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID, "label": p.Label})
	case "tx_remove_label":
		var p backendplugin.LabelParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.RemoveLabel(ctx, p.ID, p.Label, p.Actor); err != nil {
			return errorResponse(req.ID, "tx_remove_label_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.ID, "label": p.Label})
	case "tx_get_labels":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		labels, err := tx.GetLabels(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "tx_get_labels_failed", err)
		}
		return ok(req.ID, labels)
	case "tx_set_config":
		var p backendplugin.ConfigParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.SetConfig(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "tx_set_config_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "tx_get_config":
		var p backendplugin.ConfigParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		value, err := tx.GetConfig(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "tx_get_config_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "tx_set_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.SetMetadata(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "tx_set_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "tx_get_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		value, err := tx.GetMetadata(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "tx_get_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "tx_set_local_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.SetLocalMetadata(ctx, p.Key, p.Value); err != nil {
			return errorResponse(req.ID, "tx_set_local_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key})
	case "tx_get_local_metadata":
		var p backendplugin.MetadataParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		value, err := tx.GetLocalMetadata(ctx, p.Key)
		if err != nil {
			return errorResponse(req.ID, "tx_get_local_metadata_failed", err)
		}
		return ok(req.ID, map[string]string{"key": p.Key, "value": value})
	case "tx_add_comment":
		var p backendplugin.CommentParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		if err := tx.AddComment(ctx, p.IssueID, p.Author, p.Text); err != nil {
			return errorResponse(req.ID, "tx_add_comment_failed", err)
		}
		return ok(req.ID, map[string]string{"id": p.IssueID})
	case "tx_import_issue_comment":
		var p backendplugin.CommentParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		comment, err := tx.ImportIssueComment(ctx, p.IssueID, p.Author, p.Text, p.CreatedAt)
		if err != nil {
			return errorResponse(req.ID, "tx_import_issue_comment_failed", err)
		}
		return ok(req.ID, comment)
	case "tx_get_issue_comments":
		var p backendplugin.IssueIDParams
		if err := decode(req.Params, &p); err != nil {
			return errorResponse(req.ID, "bad_params", err)
		}
		tx, err := manager.GetTransaction(p.SessionID)
		if err != nil {
			return errorResponse(req.ID, "unknown_transaction", err)
		}
		comments, err := tx.GetIssueComments(ctx, p.ID)
		if err != nil {
			return errorResponse(req.ID, "tx_get_issue_comments_failed", err)
		}
		return ok(req.ID, comments)
	default:
		return errorResponse(req.ID, "unknown_method", fmt.Errorf("%s", req.Method))
	}
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, out)
}

func ok(id string, payload any) backendplugin.Response {
	return backendplugin.Response{ID: id, OK: true, Result: mustJSON(payload)}
}

func errorResponse(id, code string, err error) backendplugin.Response {
	return backendplugin.Response{
		ID: id,
		OK: false,
		Error: &backendplugin.Error{
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
