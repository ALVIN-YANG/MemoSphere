-- SQLite layout shared with existing MemoSphere databases.
CREATE TABLE IF NOT EXISTS agents (
                    token_hash TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
                    user_id TEXT NOT NULL, projects TEXT NOT NULL,
                    global_access INTEGER NOT NULL, sensitive_access INTEGER NOT NULL,
                    revoked INTEGER NOT NULL DEFAULT 0
                , read_access INTEGER NOT NULL DEFAULT 1, write_access INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS messages (
                    id INTEGER PRIMARY KEY, user_id TEXT NOT NULL,
                    agent_id TEXT NOT NULL, session_id TEXT NOT NULL,
                    source_id TEXT NOT NULL, project_id TEXT,
                    role TEXT NOT NULL, text TEXT NOT NULL,
                    sensitive INTEGER NOT NULL, recorded_at REAL NOT NULL, spoken_at TEXT, stream_id TEXT, content_state TEXT NOT NULL DEFAULT 'raw', content_revision INTEGER NOT NULL DEFAULT 1, content_hash TEXT, raw_bytes INTEGER NOT NULL DEFAULT 0, managed INTEGER NOT NULL DEFAULT 1,
                    UNIQUE(user_id, agent_id, session_id, source_id)
                );
CREATE TABLE IF NOT EXISTS users (
                    id TEXT PRIMARY KEY, email TEXT UNIQUE NOT NULL,
                    password_hash TEXT NOT NULL, created_at REAL NOT NULL
                );
CREATE TABLE IF NOT EXISTS email_challenges (
                    email TEXT NOT NULL, purpose TEXT NOT NULL, requested_at REAL NOT NULL,
                    delivery_id TEXT, code_hash TEXT, expires_at REAL, attempts INTEGER NOT NULL DEFAULT 0,
                    PRIMARY KEY(email, purpose)
                );
CREATE TABLE IF NOT EXISTS sessions (
                    token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    csrf TEXT NOT NULL, expires_at REAL NOT NULL
                );
CREATE TABLE IF NOT EXISTS projects (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    name TEXT NOT NULL, created_at REAL NOT NULL
                );
CREATE TABLE IF NOT EXISTS connections (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    name TEXT NOT NULL, kind TEXT NOT NULL, token_hash TEXT NOT NULL,
                    created_at REAL NOT NULL, last_seen REAL,
                    last_ingested REAL, revoked_at REAL
                );
CREATE TABLE IF NOT EXISTS setup_codes (
                    digest TEXT PRIMARY KEY, connection_id TEXT NOT NULL,
                    token_hash TEXT NOT NULL, expires_at REAL NOT NULL,
                    used_at REAL
                );
CREATE TABLE IF NOT EXISTS memories (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL, project_id TEXT,
                    text TEXT NOT NULL, category TEXT NOT NULL, tags TEXT NOT NULL,
                    sensitive INTEGER NOT NULL, fact_time TEXT NOT NULL,
                    revision INTEGER NOT NULL, created_at REAL NOT NULL,
                    updated_at REAL NOT NULL
                , state TEXT NOT NULL DEFAULT 'active', origin TEXT NOT NULL DEFAULT 'manual', kind TEXT NOT NULL DEFAULT 'profile', last_operation TEXT NOT NULL DEFAULT 'ADD', basis TEXT NOT NULL DEFAULT 'unverified');
CREATE TABLE IF NOT EXISTS memory_versions (
                    memory_id TEXT NOT NULL, revision INTEGER NOT NULL,
                    content TEXT NOT NULL, reason TEXT NOT NULL,
                    recorded_at REAL NOT NULL, disposition TEXT NOT NULL DEFAULT 'current', operation TEXT NOT NULL DEFAULT 'USER_EDIT', PRIMARY KEY(memory_id, revision)
                );
CREATE TABLE IF NOT EXISTS events (
                    id INTEGER PRIMARY KEY, user_id TEXT NOT NULL,
                    action TEXT NOT NULL, subject TEXT NOT NULL,
                    details TEXT NOT NULL, created_at REAL NOT NULL
                );
CREATE TABLE IF NOT EXISTS memory_dependencies (
            child TEXT NOT NULL, revision INTEGER NOT NULL, parent TEXT NOT NULL,
            parent_revision INTEGER NOT NULL, PRIMARY KEY(child,revision,parent)
        );
CREATE TABLE IF NOT EXISTS memory_reviews (
            child TEXT PRIMARY KEY, revision INTEGER NOT NULL, retry_at REAL NOT NULL DEFAULT 0
        );
CREATE TABLE IF NOT EXISTS memory_vectors (
            memory_id TEXT PRIMARY KEY, revision INTEGER NOT NULL, model TEXT NOT NULL, vector BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS memory_entities (
            memory_id TEXT NOT NULL, revision INTEGER NOT NULL, entity_id TEXT NOT NULL,
            name TEXT NOT NULL, kind TEXT NOT NULL, aliases TEXT NOT NULL,
            PRIMARY KEY(memory_id,revision,entity_id));
CREATE TABLE IF NOT EXISTS memory_epochs (user_id TEXT PRIMARY KEY, epoch INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS memory_sources (
                    memory_id TEXT NOT NULL, revision INTEGER NOT NULL, message_id INTEGER NOT NULL,
                    quote TEXT NOT NULL, source_start INTEGER, source_end INTEGER, PRIMARY KEY(memory_id,revision,message_id,quote)
                );
CREATE TABLE IF NOT EXISTS extraction_jobs (
                    message_id INTEGER PRIMARY KEY, state TEXT NOT NULL DEFAULT 'pending',
                    attempts INTEGER NOT NULL DEFAULT 0, next_attempt REAL NOT NULL DEFAULT 0,
                    lease_until REAL NOT NULL DEFAULT 0, lease_token TEXT, error TEXT,
                    result TEXT, updated_at REAL NOT NULL
                );
CREATE TABLE IF NOT EXISTS model_usage (
                    id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL, model TEXT NOT NULL,
                    input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
                    reasoning_tokens INTEGER NOT NULL, latency REAL NOT NULL, created_at REAL NOT NULL
                , outcome TEXT NOT NULL DEFAULT 'unknown', purpose TEXT NOT NULL DEFAULT 'extraction');
CREATE TABLE IF NOT EXISTS deleted_sources (
                    user_id TEXT NOT NULL, agent_id TEXT NOT NULL, session_id TEXT NOT NULL,
                    source_id TEXT NOT NULL, PRIMARY KEY(user_id,agent_id,session_id,source_id)
                );
CREATE TABLE IF NOT EXISTS conversation_streams (
            id TEXT PRIMARY KEY,user_id TEXT NOT NULL,agent_id TEXT NOT NULL,session_id TEXT NOT NULL,
            project_id TEXT,sensitive INTEGER NOT NULL,summary TEXT NOT NULL DEFAULT '[]',
            summary_bytes INTEGER NOT NULL DEFAULT 0,summary_revision INTEGER NOT NULL DEFAULT 0,
            covered_messages INTEGER NOT NULL DEFAULT 0,covered_through INTEGER NOT NULL DEFAULT 0,
            last_activity REAL NOT NULL,updated_at REAL NOT NULL
        );
CREATE TABLE IF NOT EXISTS compaction_jobs (
            stream_id TEXT PRIMARY KEY,state TEXT NOT NULL DEFAULT 'idle',attempts INTEGER NOT NULL DEFAULT 0,
            next_attempt REAL NOT NULL DEFAULT 0,lease_until REAL NOT NULL DEFAULT 0,lease_token TEXT,
            error TEXT,updated_at REAL NOT NULL
        );

CREATE TABLE IF NOT EXISTS entity_bindings (
 memory_id TEXT NOT NULL, revision INTEGER NOT NULL, entity_id TEXT NOT NULL,
 qualifier TEXT NOT NULL DEFAULT '', proof_id TEXT, proof_revision INTEGER,
 PRIMARY KEY(memory_id,revision,entity_id));

CREATE TABLE IF NOT EXISTS retain_operations (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL, agent_id TEXT NOT NULL, project_id TEXT,
 session_id TEXT NOT NULL, request_key TEXT NOT NULL, fingerprint TEXT NOT NULL,
 item_count INTEGER NOT NULL, sensitive INTEGER NOT NULL, batch_checked INTEGER NOT NULL DEFAULT 0,
 UNIQUE(user_id,agent_id,request_key));
CREATE TABLE IF NOT EXISTS retain_items (
 operation_id TEXT NOT NULL, message_id INTEGER NOT NULL, PRIMARY KEY(operation_id,message_id));

CREATE TABLE IF NOT EXISTS retain_admissions (message_id INTEGER PRIMARY KEY, spans TEXT NOT NULL);
