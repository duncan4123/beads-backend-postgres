package provider

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	backendplugin "github.com/steveyegge/beads/backend/plugin"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/pgdialect"
	backendpostgres "github.com/steveyegge/beads/internal/storage/postgres"
	"gopkg.in/yaml.v3"
)

const Name = "postgres"

type Capabilities = backendplugin.Capabilities

type Diagnostic = backendplugin.Diagnostic

func BackendCapabilities() Capabilities {
	return Capabilities{
		Embedded:          false,
		Transactions:      true,
		RawSQL:            true,
		Leases:            true,
		Maintenance:       false,
		Versioning:        false,
		Branching:         false,
		DoltRemotes:       false,
		ConcurrentWriters: true,
	}
}

func Doctor() []Diagnostic {
	return []Diagnostic{
		{
			Level:   "info",
			Code:    "protocol",
			Message: "Postgres backend plugin process protocol is available.",
		},
	}
}

type Manager struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	stores       map[storeKey]*storeEntry
	transactions map[string]*activeTransaction
}

type Session struct {
	ID       string
	BeadsDir string
	Database string
	Branch   string
	Store    *postgresStore
	storeRef *storeEntry
}

type storeKey struct {
	beadsDir string
	database string
	branch   string
}

type storeEntry struct {
	key   storeKey
	store *postgresStore
	refs  int
}

type postgresStore struct {
	*backendpostgres.Store
	beadsDir string
}

func (s *postgresStore) Path() string { return s.beadsDir }

func (s *postgresStore) CLIDir() string { return "" }

func (s *postgresStore) UnderlyingDB() *sql.DB { return s.DB() }

func (s *postgresStore) RenameCounterPrefix(ctx context.Context, oldPrefix, newPrefix string) error {
	return unsupported("RenameCounterPrefix")
}

func (s *postgresStore) RenameDependencyPrefix(ctx context.Context, oldPrefix, newPrefix string) error {
	return unsupported("RenameDependencyPrefix")
}

func (s *postgresStore) DoltGC(ctx context.Context) error { return unsupported("DoltGC") }

func (s *postgresStore) Flatten(ctx context.Context) error { return unsupported("Flatten") }

func (s *postgresStore) Compact(ctx context.Context, initialHash, boundaryHash string, oldCommits int, recentHashes []string) error {
	return unsupported("Compact")
}

func (s *postgresStore) BackupAdd(ctx context.Context, name, url string) error {
	return unsupported("BackupAdd")
}

func (s *postgresStore) BackupSync(ctx context.Context, name string) error {
	return unsupported("BackupSync")
}

func (s *postgresStore) BackupRemove(ctx context.Context, name string) error {
	return unsupported("BackupRemove")
}

func (s *postgresStore) BackupDatabase(ctx context.Context, dir string) error {
	return unsupported("BackupDatabase")
}

func (s *postgresStore) RestoreDatabase(ctx context.Context, dir string, force bool) error {
	return unsupported("RestoreDatabase")
}

func unsupported(op string) error {
	return fmt.Errorf("operation %q not supported by the postgres backend", op)
}

var errTransactionRollback = errors.New("transaction rolled back")

type activeTransaction struct {
	ID       string
	Session  *Session
	ready    chan struct{}
	done     chan transactionDecision
	finished chan error
	tx       backendplugin.Transaction
}

type transactionDecision struct {
	rollback bool
	err      error
}

func NewManager() *Manager {
	return &Manager{
		sessions:     make(map[string]*Session),
		stores:       make(map[storeKey]*storeEntry),
		transactions: make(map[string]*activeTransaction),
	}
}

func (m *Manager) Init(ctx context.Context, beadsDir, database, branch, prefix, actor string) (*Session, error) {
	if prefix = strings.TrimSpace(prefix); prefix == "" {
		prefix = "bd"
	}
	if actor = strings.TrimSpace(actor); actor == "" {
		actor = "bd-backend-postgres"
	}
	s, err := m.open(ctx, beadsDir, database, branch, true)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		_ = m.Close(s.ID)
		return nil, fmt.Errorf("set issue_prefix: %w", err)
	}
	if err := s.Store.CommitWithConfig(ctx, "bd init"); err != nil {
		_ = m.Close(s.ID)
		return nil, fmt.Errorf("commit init: %w", err)
	}
	return s, nil
}

func (m *Manager) Open(ctx context.Context, beadsDir, database, branch string) (*Session, error) {
	return m.open(ctx, beadsDir, database, branch, false)
}

func (m *Manager) open(ctx context.Context, beadsDir, database, branch string, bootstrap bool) (*Session, error) {
	if strings.TrimSpace(beadsDir) == "" {
		return nil, errors.New("beads_dir is required")
	}
	if strings.TrimSpace(database) == "" {
		database = "beads"
	}
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	absBeadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return nil, err
	}
	if !bootstrap {
		key := storeKey{beadsDir: absBeadsDir, database: database, branch: branch}
		if s := m.acquireCachedSession(key); s != nil {
			return s, nil
		}
	}
	store, err := openPostgresStore(ctx, absBeadsDir, database, bootstrap)
	if err != nil {
		return nil, err
	}
	if !bootstrap {
		if err := ensureConfigFromFile(ctx, store, absBeadsDir); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	s := &Session{
		ID:       newSessionID(),
		BeadsDir: absBeadsDir,
		Database: database,
		Branch:   branch,
		Store:    store,
	}

	m.mu.Lock()
	if !bootstrap {
		key := storeKey{beadsDir: absBeadsDir, database: database, branch: branch}
		if existing, ok := m.stores[key]; ok {
			existing.refs++
			s.Store = existing.store
			s.storeRef = existing
			m.sessions[s.ID] = s
			m.mu.Unlock()
			_ = store.Close()
			return s, nil
		}
		entry := &storeEntry{key: key, store: store, refs: 1}
		m.stores[key] = entry
		s.storeRef = entry
	}
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s, nil
}

func openPostgresStore(ctx context.Context, beadsDir, schema string, bootstrap bool) (*postgresStore, error) {
	if bootstrap {
		dsn := firstNonEmptyEnv("BEADS_POSTGRES_URL", "GC_POSTGRES_URL")
		if dsn == "" {
			return nil, errors.New("postgres init requires BEADS_POSTGRES_URL or GC_POSTGRES_URL")
		}
		if schema = strings.TrimSpace(schema); schema == "" {
			schema = firstNonEmptyEnv("BEADS_POSTGRES_SCHEMA", "GC_POSTGRES_SCHEMA")
		}
		if schema = strings.TrimSpace(schema); schema == "" {
			return nil, errors.New("postgres init requires a schema/database")
		}
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
			return nil, fmt.Errorf("create beads dir: %w", err)
		}
		redacted, err := pgdialect.RedactPassword(dsn)
		if err != nil {
			return nil, fmt.Errorf("redact postgres dsn: %w", err)
		}
		cfg := configfile.DefaultConfig()
		cfg.Backend = configfile.BackendPostgres
		cfg.Database = schema
		cfg.PostgresDSN = redacted
		cfg.PostgresSchema = schema
		if err := cfg.Save(beadsDir); err != nil {
			return nil, fmt.Errorf("write postgres metadata: %w", err)
		}
		opened, err := backendpostgres.Provision(ctx, dsn, schema)
		if err != nil {
			return nil, err
		}
		store, ok := opened.(*backendpostgres.Store)
		if !ok {
			_ = opened.Close()
			return nil, fmt.Errorf("postgres provision returned %T, want *postgres.Store", opened)
		}
		return &postgresStore{Store: store, beadsDir: beadsDir}, nil
	}
	opened, err := backendpostgres.NewFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, err
	}
	store, ok := opened.(*backendpostgres.Store)
	if !ok {
		_ = opened.Close()
		return nil, fmt.Errorf("postgres open returned %T, want *postgres.Store", opened)
	}
	return &postgresStore{Store: store, beadsDir: beadsDir}, nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func (m *Manager) acquireCachedSession(key storeKey) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.stores[key]
	if !ok {
		return nil
	}
	entry.refs++
	s := &Session{
		ID:       newSessionID(),
		BeadsDir: key.beadsDir,
		Database: key.database,
		Branch:   key.branch,
		Store:    entry.store,
		storeRef: entry,
	}
	m.sessions[s.ID] = s
	return s
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("unknown session: %s", id)
	}
	return s, nil
}

func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session: %s", id)
	}
	if s.storeRef != nil {
		m.releaseCachedStore(s.storeRef)
		return nil
	}
	return s.Store.Close()
}

func (m *Manager) releaseCachedStore(entry *storeEntry) {
	m.mu.Lock()
	entry.refs--
	if entry.refs > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.stores, entry.key)
	m.mu.Unlock()
	_ = entry.store.Close()
}

func (m *Manager) CloseAll() error {
	m.mu.Lock()
	transactions := make([]*activeTransaction, 0, len(m.transactions))
	for id, tx := range m.transactions {
		delete(m.transactions, id)
		transactions = append(transactions, tx)
	}
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		delete(m.sessions, id)
		sessions = append(sessions, s)
	}
	stores := make([]*storeEntry, 0, len(m.stores))
	for key, entry := range m.stores {
		delete(m.stores, key)
		stores = append(stores, entry)
	}
	m.mu.Unlock()
	var err error
	for _, tx := range transactions {
		if closeErr := tx.finish(context.Background(), transactionDecision{rollback: true}); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	for _, s := range sessions {
		if s.storeRef != nil {
			continue
		}
		if closeErr := s.Store.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	for _, entry := range stores {
		if closeErr := entry.store.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}

func (m *Manager) BeginTransaction(ctx context.Context, sessionID, commitMsg string) (string, error) {
	s, err := m.Get(sessionID)
	if err != nil {
		return "", err
	}
	txID := newSessionID()
	tx := &activeTransaction{
		ID:       txID,
		Session:  s,
		ready:    make(chan struct{}),
		done:     make(chan transactionDecision, 1),
		finished: make(chan error, 1),
	}

	m.mu.Lock()
	m.transactions[txID] = tx
	m.mu.Unlock()

	go func() {
		err := s.Store.RunInTransaction(context.Background(), commitMsg, func(storeTx backendplugin.Transaction) error {
			tx.tx = storeTx
			close(tx.ready)
			decision := <-tx.done
			if decision.rollback {
				return errTransactionRollback
			}
			return decision.err
		})
		if errors.Is(err, errTransactionRollback) {
			err = nil
		}
		m.mu.Lock()
		delete(m.transactions, txID)
		m.mu.Unlock()
		tx.finished <- err
	}()

	select {
	case <-tx.ready:
		return txID, nil
	case err := <-tx.finished:
		if err != nil {
			return "", err
		}
		return "", errors.New("transaction ended before it became ready")
	case <-ctx.Done():
		_ = tx.finish(context.Background(), transactionDecision{rollback: true})
		return "", ctx.Err()
	}
}

func (m *Manager) GetTransaction(id string) (backendplugin.Transaction, error) {
	m.mu.Lock()
	tx, ok := m.transactions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown transaction: %s", id)
	}
	select {
	case <-tx.ready:
		if tx.tx == nil {
			return nil, fmt.Errorf("transaction not ready: %s", id)
		}
		return tx.tx, nil
	default:
		return nil, fmt.Errorf("transaction not ready: %s", id)
	}
}

func (m *Manager) CommitTransaction(ctx context.Context, id string) error {
	tx, err := m.getActiveTransaction(id)
	if err != nil {
		return err
	}
	return tx.finish(ctx, transactionDecision{})
}

func (m *Manager) RollbackTransaction(ctx context.Context, id string) error {
	tx, err := m.getActiveTransaction(id)
	if err != nil {
		return err
	}
	return tx.finish(ctx, transactionDecision{rollback: true})
}

func (m *Manager) getActiveTransaction(id string) (*activeTransaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.transactions[id]
	if !ok {
		return nil, fmt.Errorf("unknown transaction: %s", id)
	}
	return tx, nil
}

func (tx *activeTransaction) finish(ctx context.Context, decision transactionDecision) error {
	select {
	case tx.done <- decision:
	case err := <-tx.finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-tx.finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ensureConfigFromFile(ctx context.Context, store *postgresStore, beadsDir string) error {
	cfg, ok, err := configValuesFromFile(beadsDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	changed := false
	if prefix := strings.TrimSpace(cfg.issuePrefix); prefix != "" {
		current, err := store.GetConfig(ctx, "issue_prefix")
		if err != nil {
			return fmt.Errorf("read issue_prefix config: %w", err)
		}
		if strings.TrimSpace(current) == "" {
			if err := store.SetConfig(ctx, "issue_prefix", prefix); err != nil {
				return fmt.Errorf("repair issue_prefix config: %w", err)
			}
			changed = true
		}
	}
	if customTypes := strings.TrimSpace(cfg.typesCustom); customTypes != "" {
		current, err := store.GetConfig(ctx, "types.custom")
		if err != nil {
			return fmt.Errorf("read types.custom config: %w", err)
		}
		merged := mergeCommaLists(current, customTypes)
		if merged != strings.TrimSpace(current) {
			if err := store.SetConfig(ctx, "types.custom", merged); err != nil {
				return fmt.Errorf("repair types.custom config: %w", err)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := store.CommitWithConfig(ctx, "repair config from config.yaml"); err != nil {
		return fmt.Errorf("commit config repair: %w", err)
	}
	return nil
}

func ensureIssuePrefixConfig(ctx context.Context, store *postgresStore, beadsDir string) error {
	current, err := store.GetConfig(ctx, "issue_prefix")
	if err != nil {
		return fmt.Errorf("read issue_prefix config: %w", err)
	}
	if strings.TrimSpace(current) != "" {
		return nil
	}
	prefix, ok, err := issuePrefixFromConfigFile(beadsDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := store.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		return fmt.Errorf("repair issue_prefix config: %w", err)
	}
	if err := store.CommitWithConfig(ctx, "repair issue_prefix from config.yaml"); err != nil {
		return fmt.Errorf("commit issue_prefix repair: %w", err)
	}
	return nil
}

type configFileValues struct {
	issuePrefix string
	typesCustom string
}

func configValuesFromFile(beadsDir string) (configFileValues, bool, error) {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configFileValues{}, false, nil
		}
		return configFileValues{}, false, fmt.Errorf("read config.yaml for config repair: %w", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return configFileValues{}, false, fmt.Errorf("parse config.yaml for config repair: %w", err)
	}
	values := configFileValues{}
	for _, key := range []string{"issue-prefix", "issue_prefix"} {
		if value, ok := cfg[key]; ok {
			prefix := strings.TrimSpace(fmt.Sprint(value))
			if prefix != "" {
				values.issuePrefix = strings.TrimSuffix(prefix, "-")
				break
			}
		}
	}
	for _, key := range []string{"types.custom", "types_custom"} {
		if value, ok := cfg[key]; ok {
			values.typesCustom = strings.Join(splitCommaList(fmt.Sprint(value)), ",")
			break
		}
	}
	return values, true, nil
}

func issuePrefixFromConfigFile(beadsDir string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read config.yaml for issue_prefix repair: %w", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", false, fmt.Errorf("parse config.yaml for issue_prefix repair: %w", err)
	}
	for _, key := range []string{"issue-prefix", "issue_prefix"} {
		if value, ok := cfg[key]; ok {
			prefix := strings.TrimSpace(fmt.Sprint(value))
			if prefix != "" {
				return strings.TrimSuffix(prefix, "-"), true, nil
			}
		}
	}
	return "", false, nil
}

func mergeCommaLists(current, required string) string {
	return strings.Join(mergeStringLists(splitCommaList(current), splitCommaList(required)), ",")
}

func mergeStringLists(lists ...[]string) []string {
	seen := make(map[string]struct{})
	merged := make([]string, 0)
	for _, list := range lists {
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}

func splitCommaList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *Session) SetConfig(ctx context.Context, key, value string) error {
	return s.Store.SetConfig(ctx, key, value)
}

func (s *Session) GetConfig(ctx context.Context, key string) (string, error) {
	return s.Store.GetConfig(ctx, key)
}

func (s *Session) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return s.Store.GetAllConfig(ctx)
}

func (s *Session) ExecuteRawSQL(ctx context.Context, query string) (backendplugin.RawSQLResult, error) {
	db := s.Store.UnderlyingDB()
	if db == nil {
		return backendplugin.RawSQLResult{}, errors.New("underlying database not available")
	}
	if !rawSQLIsRead(query) {
		result, err := db.ExecContext(ctx, query)
		if err != nil {
			return backendplugin.RawSQLResult{}, fmt.Errorf("exec error: %w", err)
		}
		affected, _ := result.RowsAffected()
		return backendplugin.RawSQLResult{RowsAffected: affected, Read: false}, nil
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return backendplugin.RawSQLResult{}, fmt.Errorf("query error: %w", err)
	}
	defer rows.Close()

	result, err := scanRawSQLRows(rows)
	if err != nil {
		return backendplugin.RawSQLResult{}, err
	}
	result.Read = true
	return result, nil
}

func rawSQLIsRead(query string) bool {
	trimmed := strings.TrimSpace(strings.ToUpper(query))
	return strings.HasPrefix(trimmed, "SELECT") ||
		strings.HasPrefix(trimmed, "EXPLAIN") ||
		strings.HasPrefix(trimmed, "PRAGMA") ||
		strings.HasPrefix(trimmed, "SHOW") ||
		strings.HasPrefix(trimmed, "DESCRIBE") ||
		strings.HasPrefix(trimmed, "WITH")
}

func scanRawSQLRows(rows *sql.Rows) (backendplugin.RawSQLResult, error) {
	columns, err := rows.Columns()
	if err != nil {
		return backendplugin.RawSQLResult{}, fmt.Errorf("getting columns: %w", err)
	}

	allRows := make([]map[string]interface{}, 0)
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return backendplugin.RawSQLResult{}, fmt.Errorf("scanning row: %w", err)
		}
		row := make(map[string]interface{}, len(columns))
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = val
			}
		}
		allRows = append(allRows, row)
	}
	if err := rows.Err(); err != nil {
		return backendplugin.RawSQLResult{}, fmt.Errorf("reading rows: %w", err)
	}
	return backendplugin.RawSQLResult{Columns: columns, Rows: allRows, Read: true}, nil
}

func (s *Session) DeleteConfig(ctx context.Context, key string) error {
	return s.Store.DeleteConfig(ctx, key)
}

func (s *Session) GetCustomStatuses(ctx context.Context) ([]string, error) {
	return s.Store.GetCustomStatuses(ctx)
}

func (s *Session) GetCustomStatusesDetailed(ctx context.Context) ([]backendplugin.CustomStatus, error) {
	return s.Store.GetCustomStatusesDetailed(ctx)
}

func (s *Session) GetCustomTypes(ctx context.Context) ([]string, error) {
	return s.Store.GetCustomTypes(ctx)
}

func (s *Session) GetInfraTypes(ctx context.Context) map[string]bool {
	return s.Store.GetInfraTypes(ctx)
}

func (s *Session) IsInfraTypeCtx(ctx context.Context, t backendplugin.IssueType) bool {
	return s.Store.IsInfraTypeCtx(ctx, t)
}

func (s *Session) SetMetadata(ctx context.Context, key, value string) error {
	return s.Store.SetMetadata(ctx, key, value)
}

func (s *Session) GetMetadata(ctx context.Context, key string) (string, error) {
	return s.Store.GetMetadata(ctx, key)
}

func (s *Session) SetLocalMetadata(ctx context.Context, key, value string) error {
	return s.Store.SetLocalMetadata(ctx, key, value)
}

func (s *Session) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	return s.Store.GetLocalMetadata(ctx, key)
}

func (s *Session) CreateIssue(ctx context.Context, issue *backendplugin.Issue, actor string, commit bool, message string) (*backendplugin.Issue, error) {
	if issue == nil {
		return nil, errors.New("issue is required")
	}
	withDefaults(issue)
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	if err := s.Store.CreateIssue(ctx, issue, actor); err != nil {
		return nil, err
	}
	if commit {
		if message == "" {
			message = "create issue " + issue.ID
		}
		if err := s.Store.Commit(ctx, message); err != nil {
			return nil, err
		}
	}
	return s.Store.GetIssue(ctx, issue.ID)
}

func (s *Session) CreateIssues(ctx context.Context, issues []*backendplugin.Issue, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	for _, issue := range issues {
		if issue != nil {
			withDefaults(issue)
		}
	}
	return s.Store.CreateIssues(ctx, issues, actor)
}

func (s *Session) CreateIssuesWithFullOptions(ctx context.Context, issues []*backendplugin.Issue, actor string, opts backendplugin.BatchCreateOptions) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	for _, issue := range issues {
		if issue != nil {
			withDefaults(issue)
		}
	}
	return s.Store.CreateIssuesWithFullOptions(ctx, issues, actor, opts.Storage())
}

func (s *Session) GetIssue(ctx context.Context, id string) (*backendplugin.Issue, error) {
	return s.Store.GetIssue(ctx, id)
}

func (s *Session) GetIssueByExternalRef(ctx context.Context, externalRef string) (*backendplugin.Issue, error) {
	return s.Store.GetIssueByExternalRef(ctx, externalRef)
}

func (s *Session) GetIssuesByIDs(ctx context.Context, ids []string) ([]*backendplugin.Issue, error) {
	return s.Store.GetIssuesByIDs(ctx, ids)
}

func (s *Session) SearchIssues(ctx context.Context, query string, filter backendplugin.IssueFilter) ([]*backendplugin.Issue, error) {
	return s.Store.SearchIssues(ctx, query, filter)
}

func (s *Session) SearchIssuesWithCounts(ctx context.Context, query string, filter backendplugin.IssueFilter) ([]*backendplugin.IssueWithCounts, error) {
	return s.Store.SearchIssuesWithCounts(ctx, query, filter)
}

func (s *Session) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string, commit bool, message string) (*backendplugin.Issue, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	if err := NormalizeUpdatePayload(updates); err != nil {
		return nil, err
	}
	if err := s.Store.UpdateIssue(ctx, id, updates, actor); err != nil {
		return nil, err
	}
	if commit {
		if message == "" {
			message = "update issue " + id
		}
		if err := s.Store.Commit(ctx, message); err != nil {
			return nil, err
		}
	}
	return s.Store.GetIssue(ctx, id)
}

func NormalizeUpdatePayload(updates map[string]interface{}) error {
	if updates == nil {
		return nil
	}
	rawMetadata, ok := updates["metadata"]
	if !ok {
		return nil
	}
	switch rawMetadata.(type) {
	case string, []byte, json.RawMessage:
		return nil
	}
	data, err := json.Marshal(rawMetadata)
	if err != nil {
		return fmt.Errorf("marshal metadata update: %w", err)
	}
	updates["metadata"] = json.RawMessage(data)
	return nil
}

func (s *Session) ReopenIssue(ctx context.Context, id, reason, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.ReopenIssue(ctx, id, reason, actor)
}

func (s *Session) UpdateIssueType(ctx context.Context, id, issueType, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.UpdateIssueType(ctx, id, issueType, actor)
}

func (s *Session) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.CloseIssue(ctx, id, reason, actor, session)
}

func (s *Session) DeleteIssue(ctx context.Context, id string) error {
	return s.Store.DeleteIssue(ctx, id)
}

func (s *Session) DeleteIssues(ctx context.Context, ids []string, cascade, force, dryRun bool) (*backendplugin.DeleteIssuesResult, error) {
	return s.Store.DeleteIssues(ctx, ids, cascade, force, dryRun)
}

func (s *Session) DeleteIssuesBySourceRepo(ctx context.Context, sourceRepo string) (int, error) {
	return s.Store.DeleteIssuesBySourceRepo(ctx, sourceRepo)
}

func (s *Session) UpdateIssueID(ctx context.Context, oldID, newID string, issue *backendplugin.Issue, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.UpdateIssueID(ctx, oldID, newID, issue, actor)
}

func (s *Session) ClaimIssue(ctx context.Context, id, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.ClaimIssue(ctx, id, actor)
}

func (s *Session) ClaimReadyIssue(ctx context.Context, filter backendplugin.WorkFilter, actor string) (*backendplugin.Issue, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.ClaimReadyIssue(ctx, filter, actor)
}

func (s *Session) HeartbeatIssue(ctx context.Context, id, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.HeartbeatIssue(ctx, id, actor)
}

func (s *Session) ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, actor string) ([]backendplugin.ReclaimedLease, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.ReclaimExpiredLeases(ctx, olderThan, actor)
}

func (s *Session) PromoteFromEphemeral(ctx context.Context, id, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.PromoteFromEphemeral(ctx, id, actor)
}

func (s *Session) GetNextChildID(ctx context.Context, parentID string) (string, error) {
	return s.Store.GetNextChildID(ctx, parentID)
}

func (s *Session) RenameCounterPrefix(ctx context.Context, oldPrefix, newPrefix string) error {
	return s.Store.RenameCounterPrefix(ctx, oldPrefix, newPrefix)
}

func (s *Session) RenameDependencyPrefix(ctx context.Context, oldPrefix, newPrefix string) error {
	return s.Store.RenameDependencyPrefix(ctx, oldPrefix, newPrefix)
}

func (s *Session) AddDependency(ctx context.Context, dep *backendplugin.Dependency, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.AddDependency(ctx, dep, actor)
}

func (s *Session) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.RemoveDependency(ctx, issueID, dependsOnID, actor)
}

func (s *Session) GetDependencies(ctx context.Context, id string) ([]*backendplugin.Issue, error) {
	return s.Store.GetDependencies(ctx, id)
}

func (s *Session) GetDependents(ctx context.Context, id string) ([]*backendplugin.Issue, error) {
	return s.Store.GetDependents(ctx, id)
}

func (s *Session) GetDependencyTree(ctx context.Context, id string, maxDepth int, showAllPaths, reverse bool) ([]*backendplugin.TreeNode, error) {
	return s.Store.GetDependencyTree(ctx, id, maxDepth, showAllPaths, reverse)
}

func (s *Session) AddLabel(ctx context.Context, id, label, actor string, commit bool, message string) ([]string, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	if err := s.Store.AddLabel(ctx, id, label, actor); err != nil {
		return nil, err
	}
	if commit {
		if message == "" {
			message = "add label " + label + " to " + id
		}
		if err := s.Store.Commit(ctx, message); err != nil {
			return nil, err
		}
	}
	return s.Store.GetLabels(ctx, id)
}

func (s *Session) RemoveLabel(ctx context.Context, id, label, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.RemoveLabel(ctx, id, label, actor)
}

func (s *Session) GetLabels(ctx context.Context, id string) ([]string, error) {
	return s.Store.GetLabels(ctx, id)
}

func (s *Session) GetIssuesByLabel(ctx context.Context, label string) ([]*backendplugin.Issue, error) {
	return s.Store.GetIssuesByLabel(ctx, label)
}

func (s *Session) GetDependenciesWithMetadata(ctx context.Context, id string) ([]*backendplugin.IssueWithDependencyMetadata, error) {
	return s.Store.GetDependenciesWithMetadata(ctx, id)
}

func (s *Session) GetDependentsWithMetadata(ctx context.Context, id string) ([]*backendplugin.IssueWithDependencyMetadata, error) {
	return s.Store.GetDependentsWithMetadata(ctx, id)
}

func (s *Session) GetDependencyRecords(ctx context.Context, id string) ([]*backendplugin.Dependency, error) {
	return s.Store.GetDependencyRecords(ctx, id)
}

func (s *Session) GetDependencyRecordsForIssues(ctx context.Context, ids []string) (map[string][]*backendplugin.Dependency, error) {
	return s.Store.GetDependencyRecordsForIssues(ctx, ids)
}

func (s *Session) GetAllDependencyRecords(ctx context.Context) (map[string][]*backendplugin.Dependency, error) {
	return s.Store.GetAllDependencyRecords(ctx)
}

func (s *Session) GetDependencyCounts(ctx context.Context, ids []string) (map[string]*backendplugin.DependencyCounts, error) {
	return s.Store.GetDependencyCounts(ctx, ids)
}

func (s *Session) GetBlockingInfoForIssues(ctx context.Context, ids []string) (map[string][]string, map[string][]string, map[string]string, error) {
	return s.Store.GetBlockingInfoForIssues(ctx, ids)
}

func (s *Session) IsBlocked(ctx context.Context, id string) (bool, []string, error) {
	return s.Store.IsBlocked(ctx, id)
}

func (s *Session) GetNewlyUnblockedByClose(ctx context.Context, id string) ([]*backendplugin.Issue, error) {
	return s.Store.GetNewlyUnblockedByClose(ctx, id)
}

func (s *Session) DetectCycles(ctx context.Context) ([][]*backendplugin.Issue, error) {
	return s.Store.DetectCycles(ctx)
}

func (s *Session) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	return s.Store.FindWispDependentsRecursive(ctx, ids)
}

func (s *Session) CountDependentsByStatus(ctx context.Context, id string, status backendplugin.Status) (int64, error) {
	return s.Store.CountDependentsByStatus(ctx, id, status)
}

func (s *Session) AddIssueComment(ctx context.Context, id, author, text string) (*backendplugin.Comment, error) {
	if author == "" {
		author = "bd-backend-postgres"
	}
	return s.Store.AddIssueComment(ctx, id, author, text)
}

func (s *Session) AddComment(ctx context.Context, id, actor, comment string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.AddComment(ctx, id, actor, comment)
}

func (s *Session) ImportIssueComment(ctx context.Context, id, author, text string, createdAt time.Time) (*backendplugin.Comment, error) {
	if author == "" {
		author = "bd-backend-postgres"
	}
	return s.Store.ImportIssueComment(ctx, id, author, text, createdAt)
}

func (s *Session) GetIssueComments(ctx context.Context, id string) ([]*backendplugin.Comment, error) {
	return s.Store.GetIssueComments(ctx, id)
}

func (s *Session) GetCommentCounts(ctx context.Context, ids []string) (map[string]int, error) {
	return s.Store.GetCommentCounts(ctx, ids)
}

func (s *Session) GetCommentsForIssues(ctx context.Context, ids []string) (map[string][]*backendplugin.Comment, error) {
	return s.Store.GetCommentsForIssues(ctx, ids)
}

func (s *Session) GetLabelsForIssues(ctx context.Context, ids []string) (map[string][]string, error) {
	return s.Store.GetLabelsForIssues(ctx, ids)
}

func (s *Session) GetEvents(ctx context.Context, id string, limit int) ([]*backendplugin.Event, error) {
	return s.Store.GetEvents(ctx, id, limit)
}

func (s *Session) GetAllEventsSince(ctx context.Context, since time.Time) ([]*backendplugin.Event, error) {
	return s.Store.GetAllEventsSince(ctx, since)
}

func (s *Session) ReadyWork(ctx context.Context, filter backendplugin.WorkFilter) ([]*backendplugin.Issue, error) {
	return s.Store.GetReadyWork(ctx, filter)
}

func (s *Session) ReadyWorkWithCounts(ctx context.Context, filter backendplugin.WorkFilter) ([]*backendplugin.IssueWithCounts, error) {
	return s.Store.GetReadyWorkWithCounts(ctx, filter)
}

func (s *Session) BlockedIssues(ctx context.Context, filter backendplugin.WorkFilter) ([]*backendplugin.BlockedIssue, error) {
	return s.Store.GetBlockedIssues(ctx, filter)
}

func (s *Session) EpicsEligibleForClosure(ctx context.Context) ([]*backendplugin.EpicStatus, error) {
	return s.Store.GetEpicsEligibleForClosure(ctx)
}

func (s *Session) ListWisps(ctx context.Context, filter backendplugin.WispFilter) ([]*backendplugin.Issue, error) {
	return s.Store.ListWisps(ctx, filter)
}

func (s *Session) CountIssues(ctx context.Context, query string, filter backendplugin.IssueFilter) (int64, error) {
	return s.Store.CountIssues(ctx, query, filter)
}

func (s *Session) CountIssuesByGroup(ctx context.Context, filter backendplugin.IssueFilter, groupBy string) (map[string]int, error) {
	return s.Store.CountIssuesByGroup(ctx, filter, groupBy)
}

func (s *Session) CountDependents(ctx context.Context, id string) (int64, error) {
	return s.Store.CountDependents(ctx, id)
}

func (s *Session) CountDependencies(ctx context.Context, id string) (int64, error) {
	return s.Store.CountDependencies(ctx, id)
}

func (s *Session) CountIssueComments(ctx context.Context, id string) (int64, error) {
	return s.Store.CountIssueComments(ctx, id)
}

func (s *Session) CountEvents(ctx context.Context, id string, limit int) (int64, error) {
	return s.Store.CountEvents(ctx, id, limit)
}

func (s *Session) Statistics(ctx context.Context) (*backendplugin.Statistics, error) {
	return s.Store.GetStatistics(ctx)
}

func (s *Session) GetRepoMtime(ctx context.Context, repoPath string) (int64, error) {
	return s.Store.GetRepoMtime(ctx, repoPath)
}

func (s *Session) SetRepoMtime(ctx context.Context, repoPath, jsonlPath string, mtimeNS int64) error {
	return s.Store.SetRepoMtime(ctx, repoPath, jsonlPath, mtimeNS)
}

func (s *Session) ClearRepoMtime(ctx context.Context, repoPath string) error {
	return s.Store.ClearRepoMtime(ctx, repoPath)
}

func (s *Session) GetMoleculeProgress(ctx context.Context, moleculeID string) (*backendplugin.MoleculeProgressStats, error) {
	return s.Store.GetMoleculeProgress(ctx, moleculeID)
}

func (s *Session) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*backendplugin.MoleculeLastActivity, error) {
	return s.Store.GetMoleculeLastActivity(ctx, moleculeID)
}

func (s *Session) GetStaleIssues(ctx context.Context, filter backendplugin.StaleFilter) ([]*backendplugin.Issue, error) {
	return s.Store.GetStaleIssues(ctx, filter)
}

func (s *Session) Path() string {
	return s.Store.Path()
}

func (s *Session) CLIDir() string {
	return s.Store.CLIDir()
}

func (s *Session) DoltGC(ctx context.Context) error {
	return s.Store.DoltGC(ctx)
}

func (s *Session) Flatten(ctx context.Context) error {
	return s.Store.Flatten(ctx)
}

func (s *Session) Compact(ctx context.Context, initialHash, boundaryHash string, oldCommits int, recentHashes []string) error {
	return s.Store.Compact(ctx, initialHash, boundaryHash, oldCommits, recentHashes)
}

func (s *Session) CheckEligibility(ctx context.Context, issueID string, tier int) (bool, string, error) {
	return s.Store.CheckEligibility(ctx, issueID, tier)
}

func (s *Session) ApplyCompaction(ctx context.Context, issueID string, tier int, originalSize int, compactedSize int, commitHash string) error {
	return s.Store.ApplyCompaction(ctx, issueID, tier, originalSize, compactedSize, commitHash)
}

func (s *Session) SnapshotIssue(ctx context.Context, issueID string, tier int) error {
	return s.Store.SnapshotIssue(ctx, issueID, tier)
}

func (s *Session) GetCompactionSnapshot(ctx context.Context, issueID string) (*backendplugin.IssueSnapshot, error) {
	return s.Store.GetCompactionSnapshot(ctx, issueID)
}

func (s *Session) RestoreFromSnapshot(ctx context.Context, issueID string) (*backendplugin.IssueSnapshot, error) {
	return s.Store.RestoreFromSnapshot(ctx, issueID)
}

func (s *Session) GetTier1Candidates(ctx context.Context) ([]*backendplugin.CompactionCandidate, error) {
	return s.Store.GetTier1Candidates(ctx)
}

func (s *Session) GetTier2Candidates(ctx context.Context) ([]*backendplugin.CompactionCandidate, error) {
	return s.Store.GetTier2Candidates(ctx)
}

func (s *Session) MergeSlotCreate(ctx context.Context, actor string) (*backendplugin.Issue, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.MergeSlotCreate(ctx, actor)
}

func (s *Session) MergeSlotCheck(ctx context.Context) (*backendplugin.MergeSlotStatus, error) {
	return s.Store.MergeSlotCheck(ctx)
}

func (s *Session) MergeSlotAcquire(ctx context.Context, holder, actor string, wait bool) (*backendplugin.MergeSlotResult, error) {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.MergeSlotAcquire(ctx, holder, actor, wait)
}

func (s *Session) MergeSlotRelease(ctx context.Context, holder, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.MergeSlotRelease(ctx, holder, actor)
}

func (s *Session) SlotSet(ctx context.Context, issueID, key, value, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.SlotSet(ctx, issueID, key, value, actor)
}

func (s *Session) SlotGet(ctx context.Context, issueID, key string) (string, error) {
	return s.Store.SlotGet(ctx, issueID, key)
}

func (s *Session) SlotClear(ctx context.Context, issueID, key, actor string) error {
	if actor == "" {
		actor = "bd-backend-postgres"
	}
	return s.Store.SlotClear(ctx, issueID, key, actor)
}

func (s *Session) Commit(ctx context.Context, message string) error {
	return s.Store.Commit(ctx, message)
}

func (s *Session) CommitMergeResolution(ctx context.Context, message string) error {
	return s.Store.CommitMergeResolution(ctx, message)
}

func (s *Session) CreateBranch(ctx context.Context, name string) error {
	return s.Store.Branch(ctx, name)
}

func (s *Session) Checkout(ctx context.Context, branch string) error {
	return s.Store.Checkout(ctx, branch)
}

func (s *Session) CurrentBranch(ctx context.Context) (string, error) {
	return s.Store.CurrentBranch(ctx)
}

func (s *Session) DeleteBranch(ctx context.Context, branch string) error {
	return s.Store.DeleteBranch(ctx, branch)
}

func (s *Session) ListBranches(ctx context.Context) ([]string, error) {
	return s.Store.ListBranches(ctx)
}

func (s *Session) CommitExists(ctx context.Context, hash string) (bool, error) {
	return s.Store.CommitExists(ctx, hash)
}

func (s *Session) GetCurrentCommit(ctx context.Context) (string, error) {
	return s.Store.GetCurrentCommit(ctx)
}

func (s *Session) Status(ctx context.Context) (*backendplugin.VCStatus, error) {
	return s.Store.Status(ctx)
}

func (s *Session) Log(ctx context.Context, limit int) ([]backendplugin.CommitInfo, error) {
	return s.Store.Log(ctx, limit)
}

func (s *Session) Merge(ctx context.Context, branch string) ([]backendplugin.Conflict, error) {
	return s.Store.Merge(ctx, branch)
}

func (s *Session) GetConflicts(ctx context.Context) ([]backendplugin.Conflict, error) {
	return s.Store.GetConflicts(ctx)
}

func (s *Session) ResolveConflicts(ctx context.Context, table, strategy string) error {
	return s.Store.ResolveConflicts(ctx, table, strategy)
}

func (s *Session) History(ctx context.Context, issueID string) ([]*backendplugin.HistoryEntry, error) {
	return s.Store.History(ctx, issueID)
}

func (s *Session) AsOf(ctx context.Context, issueID, ref string) (*backendplugin.Issue, error) {
	return s.Store.AsOf(ctx, issueID, ref)
}

func (s *Session) Diff(ctx context.Context, fromRef, toRef string) ([]*backendplugin.DiffEntry, error) {
	return s.Store.Diff(ctx, fromRef, toRef)
}

func (s *Session) AddRemote(ctx context.Context, name, url string) error {
	return s.Store.AddRemote(ctx, name, url)
}

func (s *Session) RemoveRemote(ctx context.Context, name string) error {
	return s.Store.RemoveRemote(ctx, name)
}

func (s *Session) HasRemote(ctx context.Context, name string) (bool, error) {
	return s.Store.HasRemote(ctx, name)
}

func (s *Session) ListRemotes(ctx context.Context) ([]backendplugin.RemoteInfo, error) {
	return s.Store.ListRemotes(ctx)
}

func (s *Session) Push(ctx context.Context) error {
	return s.Store.Push(ctx)
}

func (s *Session) Pull(ctx context.Context) error {
	return s.Store.Pull(ctx)
}

func (s *Session) ForcePush(ctx context.Context) error {
	return s.Store.ForcePush(ctx)
}

func (s *Session) PushRemote(ctx context.Context, remote string, force bool) error {
	return s.Store.PushRemote(ctx, remote, force)
}

func (s *Session) PullRemote(ctx context.Context, remote string) error {
	return s.Store.PullRemote(ctx, remote)
}

func (s *Session) Fetch(ctx context.Context, peer string) error {
	return s.Store.Fetch(ctx, peer)
}

func (s *Session) PushTo(ctx context.Context, peer string) error {
	return s.Store.PushTo(ctx, peer)
}

func (s *Session) PullFrom(ctx context.Context, peer string) ([]backendplugin.Conflict, error) {
	return s.Store.PullFrom(ctx, peer)
}

func (s *Session) BackupAdd(ctx context.Context, name, url string) error {
	return s.Store.BackupAdd(ctx, name, url)
}

func (s *Session) BackupSync(ctx context.Context, name string) error {
	return s.Store.BackupSync(ctx, name)
}

func (s *Session) BackupRemove(ctx context.Context, name string) error {
	return s.Store.BackupRemove(ctx, name)
}

func (s *Session) BackupDatabase(ctx context.Context, dir string) error {
	return s.Store.BackupDatabase(ctx, dir)
}

func (s *Session) RestoreDatabase(ctx context.Context, dir string, force bool) error {
	return s.Store.RestoreDatabase(ctx, dir, force)
}

func (s *Session) AddFederationPeer(ctx context.Context, peer *backendplugin.FederationPeer) error {
	return s.Store.AddFederationPeer(ctx, peer)
}

func (s *Session) GetFederationPeer(ctx context.Context, name string) (*backendplugin.FederationPeer, error) {
	return s.Store.GetFederationPeer(ctx, name)
}

func (s *Session) ListFederationPeers(ctx context.Context) ([]*backendplugin.FederationPeer, error) {
	return s.Store.ListFederationPeers(ctx)
}

func (s *Session) RemoveFederationPeer(ctx context.Context, name string) error {
	return s.Store.RemoveFederationPeer(ctx, name)
}

func (s *Session) SyncStatus(ctx context.Context, peer string) (*backendplugin.SyncStatus, error) {
	return s.Store.SyncStatus(ctx, peer)
}

func withDefaults(issue *backendplugin.Issue) {
	if issue.Status == "" {
		issue.Status = backendplugin.StatusOpen
	}
	if issue.IssueType == "" {
		issue.IssueType = backendplugin.TypeTask
	}
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "session"
	}
	return hex.EncodeToString(b[:])
}
