"""Rank a snapshot of authorized current facts with FTS5, cosine and entity RRF."""

import calendar
import json
import math
import re
import sqlite3
from collections import defaultdict
from datetime import date

import numpy as np

from memosphere.embedding import MODEL_ID
from memosphere.search_entities import normalized

# Calibrated as a conservative first-stage filter for this specific BGE export.
SEMANTIC_FLOOR = 0.38
SEMANTIC_ANCHOR = 0.42
CHANNEL_LIMIT = 30
TERM = re.compile(r"[a-z0-9]+(?:[._+#-][a-z0-9]+)*|[\u3400-\u9fff]+")
RECENT = re.compile(r"(最近|近期|最新|这几天|recent|latest)", re.IGNORECASE)
PROGRESS = re.compile(
    r"(进度|进展|动态|情况|状态|更新|办得|订得|progress|status|update)", re.IGNORECASE
)


def tokens(text):
    out = []
    for token in TERM.findall(normalized(text)):
        if len(token) > 1 and "\u3400" <= token[0] <= "\u9fff":
            out.extend(token[i : i + 2] for i in range(len(token) - 1))
        else:
            out.append(token)
    return out


def fts_text(words):
    # Keep identifiers like RX-17 and rx.17 distinct under FTS5 tokenization.
    return ["t" + w.encode().hex() for w in words]


def event_interval(text, spoken_at):
    from memosphere.recall import resolve_day

    match = re.fullmatch(
        r"(\d{4})[-年](\d{1,2})(?:[-月](\d{1,2}))?(?:[日号月])?(?:晚间|晚上|晚|上午|下午)?",
        text.strip(),
    )
    try:
        if match:
            year, month = map(int, match.groups()[:2])
            if match[3]:
                day = date(year, month, int(match[3]))
                return day, day
            return date(year, month, 1), date(
                year, month, calendar.monthrange(year, month)[1]
            )
        relative = resolve_day(text, spoken_at)
        if relative:
            day = date.fromisoformat(relative["date"])
            return day, day
    except ValueError:
        pass
    return None


def rank(
    db,
    agent,
    project,
    terms,
    query,
    purpose,
    vector,
    required_ids,
    required_names,
    start,
    end,
    recent,
    today,
):
    # FTS statistics and channel top-K must have the same visibility as the reply.
    # A transient per-read FTS index prevents a hidden tenant's word frequencies
    # from influencing a visible user's rank. The durable vector cache is shared by ID.
    db.execute(
        """CREATE TEMP TABLE eligible AS SELECT m.* FROM memories m WHERE user_id=?
        AND (project_id IS ? OR (project_id IS NULL AND ?)) AND (sensitive=0 OR ?)
        AND state='active' AND basis IN ('user_statement','tool_observation')""",
        (agent["user_id"], project, agent["global_access"], agent["sensitive_access"]),
    )
    db.execute("CREATE UNIQUE INDEX temp.eligible_id ON eligible(id)")
    rows = {r["id"]: r for r in db.execute("SELECT * FROM eligible")}
    entities = defaultdict(dict)
    labels = defaultdict(set)
    for r in db.execute("""SELECT e.* FROM memory_entities e JOIN eligible m
                        ON m.id=e.memory_id AND m.revision=e.revision"""):
        names = [r["name"], *json.loads(r["aliases"])]
        entities[r["memory_id"]][r["entity_id"]] = names
        labels[r["entity_id"]].update(normalized(n) for n in names)
    for r in db.execute(
        "SELECT m.id,p.id AS pid,p.name FROM eligible m JOIN projects p ON p.id=m.project_id AND p.user_id=m.user_id"
    ):
        entities[r["id"]][r["pid"]] = [r["name"]]
        labels[r["pid"]].add(normalized(r["name"]))

    unresolved = []
    for name in required_names:
        choices = sorted(
            eid for eid, names in labels.items() if normalized(name) in names
        )
        if len(choices) != 1:
            unresolved.append({"name": name, "entity_ids": choices})
        else:
            required_ids.add(choices[0])
    if unresolved:
        status = (
            "needs_clarification"
            if any(len(n["entity_ids"]) > 1 for n in unresolved)
            else "no_match"
        )
        return (
            [],
            {},
            {},
            {
                "mode": "hybrid",
                "status": status,
                "unresolved": unresolved,
                "degraded": [],
            },
        )

    stamps = {}
    for r in db.execute(
        """SELECT m.id,s.quote,msg.spoken_at FROM eligible m JOIN memory_sources s
        ON m.id=s.memory_id AND m.revision=s.revision JOIN messages msg ON msg.id=s.message_id
        WHERE msg.user_id=? AND msg.agent_id=? AND (msg.sensitive=0 OR ?) AND msg.spoken_at IS NOT NULL
        AND ((msg.project_id IS NULL AND ?) OR msg.project_id IN (SELECT value FROM json_each(?)))
        AND m.fact_time<>'' AND instr(s.quote,m.fact_time)>0 ORDER BY msg.id""",
        (
            agent["user_id"],
            agent["agent_id"],
            agent["sensitive_access"],
            agent["global_access"],
            agent["projects"],
        ),
    ):
        stamps.setdefault(r["id"], r["spoken_at"])
    intervals = {
        i: event_interval(r["fact_time"], stamps.get(i)) for i, r in rows.items()
    }

    def allowed(i):
        if required_ids and not required_ids.issubset(entities[i]):
            return False
        if start or end:
            span = intervals[i]
            return bool(
                span and (not start or span[1] >= start) and (not end or span[0] <= end)
            )
        return True

    rows = {i: r for i, r in rows.items() if allowed(i)}
    words = list(dict.fromkeys(fts_text(tokens(" ".join([query, *terms])))))[:160]
    rankings = {}
    degraded = []
    if rows and words:
        try:
            db.execute(
                "CREATE VIRTUAL TABLE temp.corpus USING fts5(id UNINDEXED, body, names)"
            )
        except sqlite3.OperationalError as error:
            if "no such module: fts5" not in str(error).lower():
                raise
            degraded.append("bm25_unavailable")
    if rows and words and not degraded:
        docs = (
            (
                i,
                " ".join(
                    fts_text(tokens(r["text"] + " " + " ".join(json.loads(r["tags"]))))
                ),
                " ".join(
                    fts_text(
                        tokens(
                            " ".join(
                                n for eid in entities[i] for n in sorted(labels[eid])
                            )
                        )
                    )
                ),
            )
            for i, r in rows.items()
        )
        db.executemany("INSERT INTO corpus VALUES(?,?,?)", docs)
        rankings["bm25"] = [
            r[0]
            for r in db.execute(
                """SELECT id FROM corpus WHERE corpus MATCH ?
            ORDER BY bm25(corpus,0,1,1),id LIMIT ?""",
                (" OR ".join(words), CHANNEL_LIMIT),
            )
        ]

    normalized_query = normalized(query)
    hits = {
        eid
        for eid, names in labels.items()
        if eid not in required_ids
        and any(n in terms or (len(n) > 1 and n in normalized_query) for n in names)
    }
    counts = {i: len(hits.intersection(entities[i])) for i in rows}
    rankings["entity"] = sorted(
        (i for i in rows if counts[i]), key=lambda i: (-counts[i], i)
    )[:CHANNEL_LIMIT]
    indexed = 0
    semantic = {}
    if vector is not None:
        for r in db.execute(
            """SELECT v.memory_id,v.vector FROM memory_vectors v JOIN eligible m
            ON m.id=v.memory_id AND m.revision=v.revision WHERE v.model=?""",
            (MODEL_ID,),
        ):
            i = r["memory_id"]
            if i not in rows or len(r["vector"]) != vector.nbytes:
                continue
            indexed += 1
            score = float(np.dot(vector, np.frombuffer(r["vector"], dtype="<f4")))
            if math.isfinite(score) and score >= SEMANTIC_FLOOR:
                semantic[i] = score
        if not any(score >= SEMANTIC_ANCHOR for score in semantic.values()):
            semantic.clear()
        rankings["semantic"] = sorted(semantic, key=lambda i: (-semantic[i], i))[
            :CHANNEL_LIMIT
        ]
    ranks = defaultdict(dict)
    for channel, ordered in rankings.items():
        for pos, i in enumerate(ordered, 1):
            ranks[i][channel] = pos

    structured = (required_ids or start or end) and not (query.strip() or terms)
    if structured:
        for i in rows:
            ranks[i]  # Explicit filter-only lookup needs no lexical anchor.
    progress = (
        recent
        if recent is not None
        else bool(RECENT.search(query) and PROGRESS.search(query))
    )
    traces = {}
    for i, positions in ranks.items():
        base = sum(1 / (60 + pos) for pos in positions.values())
        factor = 1.0
        span = intervals[i]
        if (
            progress
            and span
            and span[0] == span[1]
            and span[1] <= today
            and rows[i]["category"] != "偏好"
        ):
            factor += 0.15 * 2 ** (-(today - span[1]).days / 14)
        traces[i] = {
            "ranks": positions,
            "rrf": base,
            "time_factor": factor,
            "score": base * factor,
        }
        if i in semantic:
            traces[i]["cosine"] = semantic[i]
        if span:
            traces[i]["event_interval"] = [d.isoformat() for d in span]
            traces[i]["event_precision"] = "day" if span[0] == span[1] else "interval"
    ordered = sorted(ranks, key=lambda i: (-traces[i]["score"], i))
    if purpose == "select":
        # A confirmed preference outranks generic event data for a choice only if
        # the router supplied evidence that it applies to this question.
        preferred = [
            i
            for i in ordered
            if rows[i]["category"] == "偏好"
            and ("bm25" in ranks[i] or "entity" in ranks[i] or "semantic" in ranks[i])
        ]
        if preferred:
            ordered = preferred
    if vector is None and (query or terms):
        degraded.append("semantic_unavailable")
    elif indexed < len(rows) and not structured:
        degraded.append("semantic_index_incomplete")
    info = {
        "mode": "hybrid",
        "status": "matched" if ordered else "no_match",
        "degraded": degraded,
        "model": MODEL_ID if vector is not None else None,
    }
    return [rows[i] for i in ordered[:5]], traces, entities, info
