"""Purpose-scoped candidate retrieval. The calling router selects the task concepts."""

from datetime import date, datetime, timedelta

from memosphere import hybrid
from memosphere.memory_safety import redact


def resolve_day(fact_time, spoken_at):
    """Resolve only explicit day-relative expressions against the original speaker offset."""
    if not spoken_at:
        return None
    for term, offset in (
        ("前天", -2),
        ("昨晚", -1),
        ("昨天", -1),
        ("今天", 0),
        ("明天", 1),
        ("明早", 1),
        ("后天", 2),
    ):
        if term in fact_time:
            day = datetime.fromisoformat(spoken_at).date() + timedelta(days=offset)
            return {"date": day.isoformat(), "expression": term}
    return None


def adjacent_causal_quote(db, source, row, agent):
    """Recover a single causal sentence split into two independently visible heads."""
    peers = db.execute("""SELECT s.quote, m.text AS utterance
        FROM memory_sources s JOIN memories f ON f.id=s.memory_id AND f.revision=s.revision
        JOIN messages m ON m.id=s.message_id
        WHERE s.message_id=? AND f.id<>? AND f.user_id=? AND f.project_id IS ?
          AND f.state='active' AND f.basis='user_statement' AND (f.sensitive=0 OR ?)
          AND m.role='user'""",
        (source["message_id"], row["id"], agent["user_id"], row["project_id"], agent["sensitive_access"]))
    for peer in peers:
        utterance = peer["utterance"]
        for event, motive in ((source["quote"], peer["quote"]), (peer["quote"], source["quote"])):
            if not event or not motive.startswith(("因为", "由于", "为了")) or event[-1] in "。！？!?；;":
                continue
            if utterance.count(event) != 1 or utterance.count(motive) != 1:
                continue
            start, end = utterance.index(event), utterance.index(motive)
            gap = utterance[start + len(event):end]
            if start+len(event) <= end and not gap.strip(" ,，"):
                return redact(utterance[start:end + len(motive)])
    return None


def authorized(database, db, key, project, connection_id):
    agent = db.execute(
        "SELECT * FROM agents WHERE token_hash=? AND revoked=0",
        (database.digest(key),),
    ).fetchone()
    if (
        not agent
        or not agent["read_access"]
        or (connection_id is not None and agent["agent_id"] != connection_id)
    ):
        raise PermissionError("无读取权限")
    database.check_project(agent, project)
    return agent


def lexical_rows(db, agent, project, terms, purpose):
    if not terms:
        return []
    scope = (
        "(project_id IS ? OR project_id IS NULL)"
        if agent["global_access"]
        else "project_id IS ?"
    )
    score = "+".join(" (instr(lower(text||' '||tags),?)>0) " for _ in terms)
    rows = db.execute(
        f"""SELECT *,({score}) AS relevance FROM memories WHERE user_id=? AND {scope}
        AND state='active' AND basis IN ('user_statement','tool_observation')
        AND (sensitive=0 OR ?) AND relevance>0 ORDER BY (category='偏好' AND ?='select') DESC,relevance DESC,updated_at DESC,id LIMIT 5""",
        [*terms, agent["user_id"], project, agent["sensitive_access"], purpose],
    ).fetchall()
    if purpose == "select" and any(r["category"] == "偏好" for r in rows):
        rows = [r for r in rows if r["category"] == "偏好"]
    return rows


def recall(
    database,
    key,
    project,
    terms,
    purpose,
    query="",
    connection_id=None,
    include_context=False,
    *,
    entity_ids=None,
    entity_names=None,
    event_from=None,
    event_to=None,
    recent_progress=None,
    as_of=None,
):
    if (
        purpose not in {"skip", "select", "cite"}
        or not isinstance(terms, list)
        or len(terms) > 12
    ):
        raise ValueError("无效的召回意图或检索词")
    if any(not isinstance(t, str) or len(t) > 80 for t in terms):
        raise ValueError("检索词过长")
    terms = list(dict.fromkeys(t.strip().casefold() for t in terms if t.strip()))
    if not isinstance(query, str) or len(query) > 4000:
        raise ValueError("问句过长")
    from memosphere.extraction import candidate_keywords

    # Include lexical evidence from the original question as well as the router's
    # expansions; compound paraphrases may have no verbatim match in the store.
    expanded_terms = list(
        dict.fromkeys(
            terms + candidate_keywords(query) + candidate_keywords(" ".join(terms))
        )
    )
    if entity_ids is None:
        entity_ids = []
    if (
        not isinstance(entity_ids, list)
        or len(entity_ids) > 12
        or any(not isinstance(i, str) or not 0 < len(i) <= 80 for i in entity_ids)
    ):
        raise ValueError("无效实体 ID")
    entity_names = [] if entity_names is None else entity_names
    if (
        not isinstance(entity_names, list)
        or len(entity_names) > 12
        or any(
            not isinstance(n, str) or not 0 < len(n.strip()) <= 80 for n in entity_names
        )
    ):
        raise ValueError("无效实体名称")
    today = date.fromisoformat(as_of) if as_of else date.today()
    start = date.fromisoformat(event_from) if event_from else None
    end = date.fromisoformat(event_to) if event_to else None
    if start and end and start > end:
        raise ValueError("事件时间范围无效")
    if not start and not end and "上周" in query:
        end = today - timedelta(days=today.weekday() + 1)
        start = end - timedelta(days=6)
    legacy = getattr(database, "retrieval_mode", "hybrid") == "lexical"
    index = getattr(database, "vector_index", None)
    vector = None
    if index and purpose != "skip" and (query.strip() or terms):
        with database.connect() as db:
            authorized(database, db, key, project, connection_id)
        vector = index.query(query.strip() or " ".join(terms))
    with database.connect() as db:
        db.execute("PRAGMA temp_store=MEMORY")
        db.execute("BEGIN")
        agent = authorized(database, db, key, project, connection_id)
        result = {"purpose": purpose, "memories": []}
        if purpose == "skip":
            return result
        traces, entities = {}, {}
        if legacy:
            if entity_ids or entity_names or start or end:
                raise ValueError("词法对照模式不支持结构化筛选")
            rows = lexical_rows(db, agent, project, expanded_terms, purpose)
            result["retrieval"] = {"mode": "lexical"}
        else:
            rows, traces, entities, result["retrieval"] = hybrid.rank(
                db,
                agent,
                project,
                terms,
                query,
                purpose,
                vector,
                set(entity_ids),
                entity_names,
                start,
                end,
                recent_progress,
                today,
            )
        for row in rows:
            item = {
                k: row[k]
                for k in (
                    "id",
                    "revision",
                    "text",
                    "basis",
                    "fact_time",
                    "created_at",
                    "updated_at",
                )
            }
            if row["id"] in traces:
                item["ranking"] = traces[row["id"]]
                item["entity_ids"] = sorted(entities.get(row["id"],{}))
            sources = db.execute(
                """SELECT m.id AS message_id,m.agent_id,m.role,m.spoken_at,m.sensitive,s.quote
                FROM memory_sources s JOIN messages m ON m.id=s.message_id WHERE s.memory_id=? AND s.revision=?
                AND m.user_id=? AND m.agent_id=? AND (m.sensitive=0 OR ?)
                AND ((m.project_id IS NULL AND ?) OR m.project_id IN (SELECT value FROM json_each(?)))
                ORDER BY m.id LIMIT 4""",
                (
                    row["id"],
                    row["revision"],
                    agent["user_id"],
                    agent["agent_id"],
                    agent["sensitive_access"],
                    agent["global_access"],
                    agent["projects"],
                ),
            ).fetchall()
            item["spoken_at"] = sources[0]["spoken_at"] if sources else None
            for source in sources:
                if row["fact_time"] and row["fact_time"] in source["quote"]:
                    resolved = resolve_day(row["fact_time"], source["spoken_at"])
                    if resolved:
                        item["event_day"] = resolved
                        break
            if purpose == "cite":
                item["quotes"] = [
                    redact(s["quote"])
                    for s in sources
                    if s["agent_id"] == agent["agent_id"]
                ]
                if include_context and sources and row["basis"] == "user_statement":
                    # Only the next independently supported clause in this utterance.
                    # The original message may also contain erased or superseded claims.
                    source = sources[0]
                    causal = adjacent_causal_quote(db, source, row, agent)
                    if causal and causal not in item["quotes"]:
                        item["quotes"].insert(0, causal)
                    neighbors = db.execute("""SELECT f.id,f.revision,s.quote FROM memory_sources s
                        JOIN memories f ON f.id=s.memory_id AND f.revision=s.revision
                        JOIN messages m ON m.id=s.message_id
                        WHERE s.message_id=:message AND f.id<>:id AND f.user_id=:owner AND f.project_id IS :project
                        AND f.state='active' AND f.basis='user_statement' AND (f.sensitive=0 OR :sensitive)
                        AND instr(m.text,:quote)>0 AND instr(m.text,s.quote)>=instr(m.text,:quote)+length(:quote)
                        AND trim(substr(m.text,instr(m.text,:quote)+length(:quote),
                            instr(m.text,s.quote)-instr(m.text,:quote)-length(:quote)), ' ，,。；;、')=''
                        ORDER BY length(s.quote) DESC LIMIT 1""",
                        {"message": source["message_id"], "id": row["id"], "owner": agent["user_id"],
                         "project": row["project_id"], "sensitive": agent["sensitive_access"], "quote": source["quote"]}).fetchall()
                    item["context_quotes"] = [
                        {"id": n["id"], "revision": n["revision"], "quote": redact(n["quote"])}
                        for n in neighbors
                    ]
            result["memories"].append(item)
        return result
