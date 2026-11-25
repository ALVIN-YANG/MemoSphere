"""Evidence-bound entity names. Only aliases from visible current claims may participate."""

import json
import unicodedata
import uuid

from memosphere.memory_safety import contains_secret, sensitive_text


def normalized(value):
    return unicodedata.normalize("NFKC", value).strip().casefold()


def migrate(db):
    db.executescript("""
        CREATE TABLE IF NOT EXISTS memory_vectors (
            memory_id TEXT PRIMARY KEY, revision INTEGER NOT NULL, model TEXT NOT NULL, vector BLOB NOT NULL);
        CREATE TABLE IF NOT EXISTS memory_entities (
            memory_id TEXT NOT NULL, revision INTEGER NOT NULL, entity_id TEXT NOT NULL,
            name TEXT NOT NULL, kind TEXT NOT NULL, aliases TEXT NOT NULL,
            PRIMARY KEY(memory_id,revision,entity_id));
        CREATE INDEX IF NOT EXISTS memory_entity_lookup ON memory_entities(entity_id,memory_id,revision);
        CREATE TRIGGER IF NOT EXISTS remove_search_data AFTER DELETE ON memories BEGIN
            DELETE FROM memory_entities WHERE memory_id=old.id;
            DELETE FROM memory_vectors WHERE memory_id=old.id;
        END;
        CREATE TRIGGER IF NOT EXISTS retire_search_data AFTER UPDATE OF state,revision ON memories
        WHEN new.revision<>old.revision OR new.state<>'active' BEGIN
            DELETE FROM memory_vectors WHERE memory_id=old.id;
            DELETE FROM memory_entities WHERE memory_id=old.id AND revision<>new.revision;
        END;
    """)


def bind_entities(db, record, items, evidence=None):
    if not isinstance(items, list) or len(items) > 16:
        raise ValueError("实体最多 16 个")
    for item in items:
        if not isinstance(item, dict) or not item.get("name"):
            raise ValueError("实体格式无效")
        name, kind, aliases = (
            item.get("name"),
            item.get("kind", "other"),
            item.get("aliases", []),
        )
        if (
            not isinstance(aliases, list)
            or len(aliases) > 8
            or not all(
                isinstance(s, str)
                and 0 < len(s.strip()) <= 80
                and not contains_secret(s)
                for s in [name, kind, *aliases]
            )
        ):
            raise ValueError("实体名称或别名无效")
        if evidence is not None and name not in evidence:
            continue
        if not record["sensitive"] and any(sensitive_text(s) for s in (name, *aliases)):
            raise ValueError("包含敏感信息的实体需由敏感条目保存")
        # A name alone cannot merge two people or places. Repeated mentions on the
        # same memory keep their ID. A caller can bind another fact to an existing ID.
        explicit = item.get("id")
        if explicit is not None and not isinstance(explicit, str):
            raise ValueError("实体 ID 需使用 UUID")
        identifier = (
            str(uuid.UUID(explicit))
            if explicit is not None
            else str(
                uuid.uuid5(
                    uuid.NAMESPACE_URL,
                    json.dumps(
                        [
                            record["user_id"],
                            record["project_id"],
                            record["id"],
                            normalized(kind),
                            normalized(name),
                        ]
                    ),
                )
            )
        )
        db.execute(
            """INSERT OR REPLACE INTO memory_entities VALUES(?,?,?,?,?,?)""",
            (
                record["id"],
                record["revision"],
                identifier,
                name.strip(),
                kind,
                json.dumps(aliases, ensure_ascii=False),
            ),
        )


def entities_for(db, memory_id, revision):
    return [
        {
            "id": r["entity_id"],
            "name": r["name"],
            "kind": r["kind"],
            "aliases": json.loads(r["aliases"]),
        }
        for r in db.execute(
            "SELECT * FROM memory_entities WHERE memory_id=? AND revision=? ORDER BY entity_id",
            (memory_id, revision),
        )
    ]
