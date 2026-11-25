#!/usr/bin/env python3
"""r23 R3：megacube_tools/redact.py 的共享向量 parity 钉住。

同一张向量表逐字镜像在 components/cmsd/src/redact.rs 的
parity_vectors_pinned 单元测试——两侧任何一侧行为漂移都会使两边同时红，
使「算法与 Rust 版一一对应」的模块声明在 CI 内可验证。r22 R1/R2
（İ 前缀偏移漂移整段泄漏 / U+212A KELVIN SIGN 关键字单侧漏掩）
正是该缺口曾经漏掉的分歧。
"""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers"))

from megacube_tools.redact import redact_line, redact_text

# (输入, 期望输出)：与 components/cmsd/src/redact.rs 的 vectors 完全一致
VECTORS = [
    ("Authorization: Bearer abcdef123456", "Authorization: ***"),
    ("Authorization:Bearer", "Authorization:***"),
    ("tachyon-serve --api-key mysecretkey123 --port 18080", "tachyon-serve --api-key *** --port 18080"),
    ('cfg={"api_key":"xyz987","port":1}', 'cfg={"api_key":"***","port":1}'),
    ("key sk-abcDEF123ghi invalid", "key sk-*** invalid"),
    ("TOKEN=tok123", "TOKEN=***"),
    ("Password: hunter2", "Password: ***"),
    ("token=a1b2 secret: c3d4", "token=*** secret: ***"),
    ("LMM_HF_TOKEN=hf_ABCDEF1234567890abcdefghij", "LMM_HF_TOKEN=***"),
    # r23 R1：İ（U+0130）lower 变长曾使掩码落点漂移直至整段泄漏
    ("\u0130" * 37 + " LMM_HF_TOKEN=hf_ABCDEF1234567890abcdefghij", "\u0130" * 37 + " LMM_HF_TOKEN=***"),
    ("\u0130 token=q", "\u0130 token=***"),
    # r23 R2：U+212A KELVIN SIGN 关键字双端均可命中
    ("TO\u212aEN=hf_SECRETVALUE", "TO\u212aEN=***"),
    ("API\u212aEY=q7z9x", "API\u212aEY=***"),
    ("s\u212a-abc123xyz", "s\u212a-***"),
    # 正文不误伤：更长的词 / 无值关键字 / sk- 词内
    ("token 桶已空", "token 桶已空"),
    ("KV 32,768 tokens, fp8, explicit", "KV 32,768 tokens, fp8, explicit"),
    ("loading tokenizer.json", "loading tokenizer.json"),
    ("task-123 done", "task-123 done"),
    ("desktop", "desktop"),
    ("token=", "token="),
    # 值域非 ASCII 终止扫描：双端一致（形态缺口在台账登记，不在此钉掩码）
    ("token=\u0130STANBUL-secret99", "token=\u0130STANBUL-secret99"),
    # 多行 / 空行
    ("TOKEN=a\npassword=b", "TOKEN=***\npassword=***"),
    ("", ""),
]


class RedactParityVectorsTest(unittest.TestCase):
    def test_vectors(self):
        for line, expected in VECTORS:
            with self.subTest(line=line[:24]):
                self.assertEqual(redact_line(line), expected)

    def test_idempotent(self):
        # 掩码结果再扫一遍不变（redact(redact(x)) == redact(x)）
        for line, expected in VECTORS:
            with self.subTest(line=line[:24]):
                self.assertEqual(redact_line(expected), expected)

    def test_text_split_lines(self):
        self.assertEqual(redact_text("TOKEN=a\n\npassword=b"), "TOKEN=***\n\npassword=***")


if __name__ == "__main__":
    unittest.main()
