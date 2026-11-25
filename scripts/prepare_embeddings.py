"""Install the pinned Chinese BGE ONNX export for offline inference."""

import argparse
import hashlib
import os
import shutil
import tempfile
from pathlib import Path
from urllib.request import urlopen

from memosphere.embedding import ARTIFACTS


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--directory", type=Path, default=Path(".runtime/bge-small-zh-v1.5")
    )
    args = parser.parse_args()
    base = "https://huggingface.co/Xenova/bge-small-zh-v1.5/resolve/75c43b069aac4d136ba6bc1122f995fedcfd2781/"
    args.directory.mkdir(parents=True, exist_ok=True)
    for name, digest in ARTIFACTS.items():
        target = args.directory / name
        if (
            target.exists()
            and hashlib.sha256(target.read_bytes()).hexdigest() == digest
        ):
            continue
        remote = "onnx/model_quantized.onnx" if name == "model.onnx" else name
        with tempfile.NamedTemporaryFile(dir=args.directory, delete=False) as file:
            temporary = Path(file.name)
        try:
            with (
                urlopen(base + remote, timeout=90) as response,
                temporary.open("wb") as file,
            ):
                shutil.copyfileobj(response, file)
            if hashlib.sha256(temporary.read_bytes()).hexdigest() != digest:
                raise ValueError(f"{name} 校验失败")
            os.replace(temporary, target)
        finally:
            temporary.unlink(missing_ok=True)
    print(f"向量模型已校验：{args.directory}")


if __name__ == "__main__":
    main()
