"""Local BGE inference and a revision-fenced SQLite vector cache."""

import asyncio
import hashlib
import logging
import re
from contextlib import asynccontextmanager
from pathlib import Path

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

ARTIFACTS = {
    "model.onnx": "15b717c382bcb518ba457b93ea6850ede7f4f1cd8937454aa06972366cd19bcc",
    "tokenizer.json": "48cea5d44424912a6fd1ea647bf4fe50b55ab8b1e5879c3275f80e339e8fae26",
}
MODEL_ID = "bge-small-zh-v1.5/75c43b06/int8-cls-l2-v1"
QUERY_PREFIX = "为这个句子生成表示以用于检索相关文章："


RECALL_PREFIX = re.compile(
    r"^\s*(?:我(?:以前|之前|先前|曾经)?(?:说过|提过)|还记得我(?:说过|提过))[，,：:、]\s*"
)


class BGE:
    def __init__(self, directory):
        directory = Path(directory)
        for filename, expected in ARTIFACTS.items():
            with (directory / filename).open("rb") as stream:
                if hashlib.file_digest(stream, "sha256").hexdigest() != expected:
                    raise ValueError(f"向量模型文件校验失败：{filename}")
        self.tokenizer = Tokenizer.from_file(str(directory / "tokenizer.json"))
        self.tokenizer.enable_truncation(max_length=512)
        self.tokenizer.enable_padding(pad_id=0, pad_token="[PAD]")
        options = ort.SessionOptions()
        options.intra_op_num_threads = 2
        options.inter_op_num_threads = 1
        self.session = ort.InferenceSession(
            str(directory / "model.onnx"), options, providers=["CPUExecutionProvider"]
        )
        self.encode(["预热"])

    def encode(self, texts):
        batch = self.tokenizer.encode_batch(texts)
        inputs = {
            name: np.asarray([getattr(b, field) for b in batch], dtype=np.int64)
            for name, field in (
                ("input_ids", "ids"),
                ("attention_mask", "attention_mask"),
                ("token_type_ids", "type_ids"),
            )
        }
        pooled = self.session.run(["last_hidden_state"], inputs)[0][:, 0, :]
        norm = np.linalg.norm(pooled, axis=1, keepdims=True)
        if pooled.shape != (len(texts), 512) or not np.all(
            np.isfinite(norm) & (norm > 0)
        ):
            raise ValueError("无效的模型输出")
        return (pooled / norm).astype("<f4")


class VectorIndex:
    def __init__(self, database, directory):
        self.database = database
        self.model = None
        try:
            self.model = BGE(directory)
        except Exception as error:  # noqa: BLE001 -- local runtime failures disable this optional channel
            logging.getLogger(__name__).warning(
                "semantic_unavailable kind=%s", type(error).__name__
            )

    def backfill(self):
        with self.database.connect() as db:
            rows = db.execute(
                """SELECT m.id,m.revision,m.text FROM memories m
                LEFT JOIN memory_vectors v ON v.memory_id=m.id AND v.revision=m.revision AND v.model=?
                WHERE m.state='active' AND m.basis IN ('user_statement','tool_observation')
                AND v.memory_id IS NULL ORDER BY m.updated_at DESC,m.id LIMIT 12""",
                (MODEL_ID,),
            ).fetchall()
        if not rows:
            return False
        vectors = self.model.encode([r["text"] for r in rows])
        with self.database.connect() as db:
            for row, vector in zip(rows, vectors, strict=True):
                db.execute(
                    """INSERT OR REPLACE INTO memory_vectors(memory_id,revision,model,vector)
                    SELECT id,revision,?,? FROM memories WHERE id=? AND revision=? AND state='active'
                    AND basis IN ('user_statement','tool_observation')""",
                    (MODEL_ID, vector.tobytes(), row["id"], row["revision"]),
                )
        return True

    async def run(self):
        while self.model:
            try:
                if await asyncio.to_thread(self.backfill):
                    continue
            except Exception as error:  # noqa: BLE001 -- native inference failures must leave lexical recall available
                logging.getLogger(__name__).warning(
                    "vector_backfill_retry kind=%s", type(error).__name__
                )
                await asyncio.sleep(2)
            await asyncio.sleep(0.3)

    def query(self, text):
        if self.model:
            try:
                subject = RECALL_PREFIX.sub("", text, count=1).strip() or text
                return self.model.encode([QUERY_PREFIX + subject])[0]
            except Exception as error:  # noqa: BLE001 -- native inference failures must leave lexical recall available
                logging.getLogger(__name__).warning(
                    "query_vector_unavailable kind=%s", type(error).__name__
                )
        return None


@asynccontextmanager
async def indexing(index):
    async with asyncio.TaskGroup() as group:
        task = group.create_task(index.run()) if index and index.model else None
        try:
            yield
        finally:
            if task:
                task.cancel()
