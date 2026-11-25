"""个人记忆、项目与历史记录；所有查询从账户身份确定所有者。"""

import json
import time
import uuid
from memosphere import evidence
from memosphere.memory_safety import contains_secret, sensitive_text


class Spaces:
    def __init__(self, database):
        self.db = database

    def projects(self, user):
        with self.db.connect() as db:
            return [dict(r) for r in db.execute("SELECT * FROM projects WHERE user_id=? ORDER BY created_at", (user,))]

    def check_projects(self, user, projects):
        if not isinstance(projects, list) or len(projects) > 100:
            raise ValueError("项目列表无效")
        owned = {p["id"] for p in self.projects(user)}
        if any(not isinstance(p, str) or p not in owned for p in projects):
            raise PermissionError("项目不存在或无权访问")

    def create_project(self, user, name):
        name = str(name).strip()
        if not 1 <= len(name) <= 80:
            raise ValueError("项目名称需为 1 至 80 个字符")
        project = {"id": str(uuid.uuid4()), "user_id": user, "name": name, "created_at": time.time()}
        with self.db.connect() as db:
            db.execute("INSERT INTO projects VALUES (:id,:user_id,:name,:created_at)", project)
            self.db.event(db, user, "project.created", project["id"], name=name)
        return project

    @staticmethod
    def decode(row):
        value = dict(row)
        value["tags"] = json.loads(value["tags"])
        value["sensitive"] = bool(value["sensitive"])
        return value

    def memories(self, user, query="", project=None, sensitive=None, offset=0, state="active",
                 *, category=None, entity=None, order="updated"):
        where, args = ["user_id=?"], [user]
        if state not in {"active", "uncertain", "suspended", "all"}:raise ValueError("记忆状态无效")
        if state != "all":where.append("state=?");args.append(state)
        if query:
            where.append("(instr(lower(text),lower(?))>0 OR instr(lower(tags),lower(?))>0)")
            args.extend([query, query])
        if project:
            where.append("project_id IS NULL" if project == "global" else "project_id=?")
            if project != "global":args.append(project)
        if category:
            where.append("category=?")
            args.append(category)
        if entity:
            where.append("""EXISTS (SELECT 1 FROM memory_entities e WHERE e.entity_id=?
                         AND e.memory_id=memories.id AND e.revision=memories.revision)""")
            args.append(entity)
        if order not in {"updated", "created"}:
            raise ValueError("排序参数无效")
        if sensitive is not None:
            where.append("sensitive=?");args.append(sensitive)
        with self.db.connect() as db:
            db.execute("BEGIN")
            sql = " FROM memories WHERE " + " AND ".join(where)
            total = db.execute("SELECT count(*)" + sql, args).fetchone()[0]
            column = "created_at" if order == "created" else "updated_at"
            rows = db.execute("SELECT *" + sql + f" ORDER BY {column} DESC, id DESC LIMIT 50 OFFSET ?", [*args, max(0, offset)])
            page = {"items": [self.decode(r) for r in rows], "total": total, "offset": max(0, offset)}
            if order == "created":
                page["categories"] = [r[0] for r in db.execute(
                    "SELECT DISTINCT category FROM memories WHERE user_id=? ORDER BY category", (user,))]
            return page

    def memory(self, user, memory_id):
        with self.db.connect() as db:
            db.execute("BEGIN")
            row = db.execute("SELECT * FROM memories WHERE user_id=? AND id=?", (user, memory_id)).fetchone()
            if not row:
                raise LookupError("记忆不存在")
            result = self.decode(row)
            result["versions"] = [{**dict(v), "content": json.loads(v["content"])} for v in db.execute(
                "SELECT * FROM memory_versions WHERE memory_id=? ORDER BY revision DESC", (memory_id,))]
            result["sources"] = [dict(r) for r in db.execute("""SELECT s.revision,s.message_id,s.quote,
                m.agent_id,m.session_id,m.role,m.recorded_at,m.spoken_at FROM memory_sources s
                JOIN messages m ON m.id=s.message_id WHERE s.memory_id=? ORDER BY s.revision DESC""", (memory_id,))]
            from memosphere.search_entities import entities_for
            result["entities"] = entities_for(db, memory_id, row["revision"])
            # Resolve the versions named by each inference, including a former inference
            # after its owner has confirmed or corrected it. Never substitute today's text.
            refs, snapshots = {}, {}
            for link in db.execute("""SELECT d.revision,d.parent,d.parent_revision,v.content,m.id AS source_id,
                    m.role,m.spoken_at,s.quote FROM memory_dependencies d
                    LEFT JOIN memories owner ON owner.id=d.parent AND owner.user_id=?
                    LEFT JOIN memory_versions v ON v.memory_id=owner.id AND v.revision=d.parent_revision
                    LEFT JOIN memory_sources s ON s.memory_id=v.memory_id AND s.revision=v.revision
                    LEFT JOIN messages m ON m.id=s.message_id AND m.user_id=?
                    WHERE d.child=? ORDER BY d.revision,d.parent,s.message_id,s.quote""", (user, user, memory_id)):
                version, parent = link["revision"], link["parent"]
                ref = {"id": parent, "revision": link["parent_revision"]}
                refs.setdefault(version, {})[parent] = ref
                if link["content"] is None:
                    continue
                current = snapshots.setdefault(version, {})
                if parent not in current:
                    current[parent] = {**ref, "text": json.loads(link["content"])["text"], "sources": []}
                if link["source_id"] is not None:
                    current[parent]["sources"].append({k: link[k] for k in ("role", "spoken_at", "quote")})
            for record in [result, *result["versions"]]:
                record["inputs"] = list(refs.get(record["revision"], {}).values())
                record["input_evidence"] = list(snapshots.get(record["revision"], {}).values())
        return result

    @staticmethod
    def bump(db, user):
        db.execute("INSERT INTO memory_epochs VALUES (?,1) ON CONFLICT(user_id) DO UPDATE SET epoch=epoch+1", (user,))

    def write(self, db, record, old, reason, operation="USER_EDIT", inputs=()):
        if inputs:
            evidence.validate_inputs(db, record, inputs)
        db.execute("DELETE FROM memory_reviews WHERE child=?", (record["id"],))
        if old:
            evidence.suspend_dependents(db, old["id"], old["revision"])
            disposition = {"UPDATE": "historical", "CORRECT": "invalidated", "UNCERTAIN": "uncertain"}.get(operation, "superseded")
            if old["state"] == "uncertain":disposition = "uncertain"
            db.execute("UPDATE memory_versions SET disposition=? WHERE memory_id=? AND revision=?",
                       (disposition, old["id"], old["revision"]))
            db.execute("""UPDATE memories SET project_id=:project_id,text=:text,category=:category,tags=:tags,
                sensitive=:sensitive,fact_time=:fact_time,revision=:revision,updated_at=:updated_at,
                state=:state,origin=:origin,kind=:kind,last_operation=:last_operation
                WHERE id=:id AND user_id=:user_id""", record)
        else:
            db.execute("""INSERT INTO memories
                (id,user_id,project_id,text,category,tags,sensitive,fact_time,revision,created_at,updated_at,state,origin,kind,last_operation)
                VALUES (:id,:user_id,:project_id,:text,:category,:tags,:sensitive,:fact_time,:revision,:created_at,:updated_at,
                        :state,:origin,:kind,:last_operation)""", record)
        db.execute("UPDATE memories SET basis=? WHERE id=?", (record["basis"], record["id"]))
        db.executemany("INSERT INTO memory_dependencies VALUES(?,?,?,?)", [
            (record["id"],record["revision"],ref["id"],ref["revision"]) for ref in inputs])
        db.execute("""INSERT INTO memory_versions(memory_id,revision,content,reason,recorded_at,disposition,operation)
            VALUES (?,?,?,?,?,'current',?)""", (record["id"], record["revision"],
            json.dumps(self.decode(record), ensure_ascii=False), reason, record["updated_at"], operation))
        self.bump(db, record["user_id"])

    def save(self, user, data, memory_id=None):
        text = str(data.get("text", "")).strip()
        if not 1 <= len(text) <= 12000:
            raise ValueError("记忆内容需为 1 至 12000 个字符")
        if contains_secret(text):raise ValueError("普通记忆不保存密码、密钥或访问令牌")
        project = data.get("project_id") or None
        if project:self.check_projects(user, [project])
        tags = data.get("tags", [])
        if not isinstance(tags, list) or len(tags) > 20 or any(not isinstance(t, str) or len(t) > 40 for t in tags):
            raise ValueError("最多添加 20 个标签，每个不超过 40 字")
        category = str(data.get("category", "其他"))[:40]
        fact_time = str(data.get("fact_time", ""))[:120]
        reason = str(data.get("reason", "用户编辑"))[:240]
        now = time.time()
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            old = None
            if memory_id:
                old = db.execute("SELECT * FROM memories WHERE id=? AND user_id=?", (memory_id, user)).fetchone()
                if not old:raise LookupError("记忆不存在")
                if data.get("revision") != old["revision"]:
                    raise ValueError("这条记忆刚被修改，请刷新后再编辑")
            record = {"id": memory_id or str(uuid.uuid4()), "user_id": user, "project_id": project,
                      "text": text, "category": category, "tags": json.dumps(list(dict.fromkeys(tags)), ensure_ascii=False),
                      "sensitive": int(bool(data.get("sensitive", False)) or sensitive_text(text)), "fact_time": fact_time,
                      "revision": old["revision"] + 1 if old else 1,
                      "created_at": old["created_at"] if old else now, "updated_at": now,
                      "state": "active", "origin": "manual", "kind": old["kind"] if old else ("project" if project else "profile"),
                      "last_operation": "USER_EDIT" if old else "ADD"}
            refs = data.get("inputs", [])
            if refs and old:
                raise ValueError("新推断须独立建立，确认已有推断时提交本人陈述")
            record["basis"] = "inference" if refs else "user_statement"
            if refs:
                record.update(state="uncertain", origin="derived", last_operation="INFER")
            elif old and old["basis"] == "inference" and data.get("confirm_statement") is not True:
                raise ValueError("请明确确认本人的陈述，再保存为已确认记忆")
            from memosphere.search_entities import bind_entities, entities_for
            bindings = data.get("entities")
            if bindings is None:
                bindings = [e for e in entities_for(db, memory_id, old["revision"])
                            if old["project_id"] == project and (e["name"] in text or any(a in text for a in e["aliases"]))] if old else []
            self.write(db, record, old, reason if old else "用户新增", "INFER" if refs else "USER_EDIT", refs)
            bind_entities(db, record, bindings)
            self.db.event(db, user, "memory.updated" if old else "memory.created", record["id"], revision=record["revision"])
        return self.memory(user, record["id"])

    def delete(self, user, memory_id):
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            target = db.execute("SELECT * FROM memories WHERE id=? AND user_id=?", (memory_id, user)).fetchone()
            if not target:
                raise LookupError("记忆不存在")
            forgotten, todo = set(), [memory_id]
            while todo:
                current = todo.pop()
                if current in forgotten:continue
                forgotten.add(current)
                todo.extend(evidence.purge_derivations(db, current, user))
                sources = db.execute("SELECT message_id,quote FROM memory_sources WHERE memory_id=?", (current,)).fetchall()
                for source in sources:
                    quote = source["quote"]
                    if not quote:continue
                    # 同一证据片段的衍生记忆一起清除，其他独立片段保持可用。
                    for related in db.execute("""SELECT s.memory_id,s.quote FROM memory_sources s
                        JOIN memories m ON m.id=s.memory_id WHERE s.message_id=? AND m.user_id=?""", (source["message_id"], user)):
                        if quote in related["quote"] or related["quote"] in quote:todo.append(related["memory_id"])
                    for message in db.execute("""SELECT m.* FROM messages m JOIN messages original ON original.id=?
                        WHERE m.user_id=? AND (m.id=original.id OR (m.agent_id=original.agent_id
                        AND m.session_id=original.session_id AND m.project_id IS original.project_id AND m.role=original.role
                        AND m.text=? AND NOT EXISTS(SELECT 1 FROM memory_sources ms WHERE ms.message_id=m.id)))""",
                        (source["message_id"],user,quote)).fetchall():
                        db.execute("UPDATE messages SET text=replace(text,?,'[用户已删除相关内容]') WHERE id=?", (quote, message["id"]))
                        db.execute("INSERT OR IGNORE INTO deleted_sources VALUES (?,?,?,?)",
                                   (user, message["agent_id"], message["session_id"], message["source_id"]))
                        db.execute("UPDATE extraction_jobs SET state='cancelled',lease_token=NULL,result=NULL,error='user_deleted' WHERE message_id=?", (message["id"],))
                db.execute("DELETE FROM memory_sources WHERE memory_id=?", (current,))
                db.execute("DELETE FROM memory_versions WHERE memory_id=?", (current,))
                db.execute("DELETE FROM memories WHERE id=? AND user_id=?", (current, user))
            # Epoch fencing invalidates snapshots taken before erasure. Unrelated pending
            # jobs are allowed to rebuild their context from the cleaned sources.
            self.bump(db, user)
            self.db.event(db, user, "memory.deleted", memory_id, deleted_memories=len(forgotten))
        return {"memories": len(forgotten)}

    def agent_memories(self, agent, project, query=""):
        if not agent.get("read_access", 1):raise PermissionError("此 Agent 没有读取权限")
        self.db.check_project(agent, project)
        where, args = ["user_id=?", "state='active'"], [agent["user_id"]]
        if project is not None and agent["global_access"]:
            where.append("(project_id=? OR project_id IS NULL)");args.append(project)
        elif project is None:where.append("project_id IS NULL")
        else:where.append("project_id=?");args.append(project)
        if not agent["sensitive_access"]:where.append("sensitive=0")
        if query:where.append("instr(lower(text),lower(?))>0");args.append(query)
        with self.db.connect() as db:
            return [self.decode(r) for r in db.execute("SELECT * FROM memories WHERE " + " AND ".join(where) +
                                                      " ORDER BY updated_at DESC LIMIT 20", args)]
