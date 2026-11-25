-- SQLite layout shared with existing MemoSphere databases.
CREATE INDEX IF NOT EXISTS messages_scope ON messages(user_id, project_id, id);
CREATE INDEX IF NOT EXISTS memories_owner ON memories(user_id,updated_at);
CREATE INDEX IF NOT EXISTS events_owner ON events(user_id,id);
CREATE INDEX IF NOT EXISTS memory_dependents ON memory_dependencies(parent,parent_revision);
CREATE INDEX IF NOT EXISTS memory_entity_lookup ON memory_entities(entity_id,memory_id,revision);
CREATE TRIGGER IF NOT EXISTS remove_search_data AFTER DELETE ON memories BEGIN
            DELETE FROM memory_entities WHERE memory_id=old.id;
            DELETE FROM memory_vectors WHERE memory_id=old.id;
        END;
DROP TRIGGER IF EXISTS retire_search_data;
CREATE TRIGGER IF NOT EXISTS retire_current_vector AFTER UPDATE OF state,revision ON memories
 WHEN new.revision<>old.revision OR new.state<>'active' BEGIN
 DELETE FROM memory_vectors WHERE memory_id=old.id;
 END;
CREATE INDEX IF NOT EXISTS entity_proof ON entity_bindings(proof_id,proof_revision);
CREATE TRIGGER IF NOT EXISTS forget_entity_bindings BEFORE DELETE ON memories BEGIN
 DELETE FROM memory_entities WHERE (memory_id,revision,entity_id) IN
 (SELECT memory_id,revision,entity_id FROM entity_bindings WHERE proof_id=old.id);
 DELETE FROM entity_bindings WHERE memory_id=old.id OR proof_id=old.id;
 END;
CREATE VIEW IF NOT EXISTS current_entity_links AS
 SELECT e.*,coalesce(b.qualifier,'') qualifier,b.proof_id,b.proof_revision
 FROM memory_entities e LEFT JOIN entity_bindings b USING(memory_id,revision,entity_id)
 WHERE b.proof_id IS NULL OR EXISTS (
 SELECT 1 FROM memories proof JOIN memories child ON child.id=e.memory_id
 WHERE proof.id=b.proof_id AND proof.revision=b.proof_revision AND proof.state='active'
 AND proof.user_id=child.user_id AND proof.project_id IS child.project_id AND proof.sensitive<=child.sensitive);
CREATE INDEX IF NOT EXISTS memory_sources_message ON memory_sources(message_id);
CREATE INDEX IF NOT EXISTS extraction_pending ON extraction_jobs(state,next_attempt,message_id);
CREATE INDEX IF NOT EXISTS message_stream ON messages(stream_id,id);
CREATE INDEX IF NOT EXISTS conversation_owner ON conversation_streams(user_id,last_activity);
