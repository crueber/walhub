#!/usr/bin/env python3
"""Dev-time hand-computation of the map-bearing golden fixtures (doc 02 §2.3.3).

protoc's C++ serializer emits map entries in insertion order and always writes both map
entry sides; doc 02 §2.3.2 mandates sorted key order with default sides omitted. These two
fixtures (entry_push.bin, maps.bin) are therefore hand-computed to the doc's wire rules and
cross-validated by decoding with `protoc --decode` (see the check at the bottom of the
fixtures script). All other fixtures use raw protoc --encode output.
"""
import subprocess, sys

def uv(n):
    out = b""
    while True:
        b = n & 0x7F
        n >>= 7
        out += bytes([b | (0x80 if n else 0)])
        if not n:
            return out

def tag(num, wt):
    return uv(num << 3 | wt)

def vfield(num, v):          # varint field, always written
    return tag(num, 0) + uv(v)

def sfield(num, s):          # string/submessage field, omitted when empty
    if s == b"":
        return b""
    return tag(num, 2) + uv(len(s)) + s

def ts(seconds, nanos):
    b = b""
    if seconds != 0:
        b += vfield(1, seconds & 0xFFFFFFFFFFFFFFFF)
    if nanos != 0:
        b += vfield(2, nanos & 0xFFFFFFFFFFFFFFFF)
    return b

def logentry(seq=0, kind=0, pack=b"", txn=b"", supersedes=(), checkpoint=b"",
             created_at=b"", writer=b"", meta=None, settings=b""):
    b = b""
    if seq:
        b += vfield(1, seq)
    if kind:
        b += vfield(2, kind)
    b += sfield(3, pack)
    b += sfield(4, txn)
    for s in supersedes:
        b += sfield(5, s.encode())
    b += sfield(6, checkpoint)
    b += sfield(7, created_at)
    b += sfield(8, writer.encode())
    for k in sorted(meta or {}):
        e = sfield(1, k.encode()) + sfield(2, meta[k].encode())
        b += tag(9, 2) + uv(len(e)) + e
    b += sfield(10, settings)
    return b

def packref(checksum, pack_size=0, idx_size=0, has_rev=False, has_bitmap=False,
            object_count=0, seq=0, tier=0, has_commit_graph=False, kind=0, derived_from=b""):
    b = sfield(1, checksum if isinstance(checksum, bytes) else checksum.encode())
    if pack_size: b += vfield(2, pack_size)
    if idx_size: b += vfield(3, idx_size)
    if has_rev: b += vfield(4, 1)
    if has_bitmap: b += vfield(5, 1)
    if object_count: b += vfield(6, object_count)
    if seq: b += vfield(7, seq)
    if tier: b += vfield(8, tier)
    if has_commit_graph: b += vfield(9, 1)
    if kind: b += vfield(10, kind)
    b += sfield(11, derived_from)
    return b

def refupdate(name=b"", old_oid=b"", new_oid=b"", new_symbolic_target=b"", new_peeled=b""):
    return (sfield(1, name) + sfield(2, old_oid) + sfield(3, new_oid)
            + sfield(4, new_symbolic_target) + sfield(5, new_peeled))

def txn(updates=(), push_options=(), atomic=False):
    b = b""
    for u in updates:
        b += tag(1, 2) + uv(len(u)) + u
    for p in push_options:
        b += sfield(2, p.encode())
    if atomic:
        b += vfield(3, 1)
    return b

Z40 = b"0" * 40

# ---- entry_push.bin: LogEntry with txn + meta (sorted map, empty sides omitted) ----
push = logentry(
    seq=41, kind=1,
    pack=packref(b"deadbeef" * 5, pack_size=1024, idx_size=256, object_count=3, seq=41),
    txn=txn(
        updates=[
            refupdate(b"refs/heads/main", Z40, b"1234567890abcdef" * 2 + b"1234"),
            refupdate(b"refs/tags/v1", new_oid=b"abcdefabcdef" * 2 + b"abcd",
                      new_peeled=b"1234567890abcdef" * 2 + b"1234"),
        ],
        push_options=["atomic", "ci.skip"], atomic=True,
    ),
    created_at=ts(1700000000, 999999999),
    writer="walhub-7f3a",
    meta={"principal": "user:alice", "request_id": "req-123", "agent": "git/2.43"},
)
open("golden/log_entry_all_kinds/entry_push.bin", "wb").write(push)

# ---- maps.bin: multi-entry meta, unsorted input, empty key and empty value ----
maps = logentry(seq=1, writer="w",
                meta={"zeta": "1", "alpha": "2", "empty": "", "": "4", "mid": "3"})
open("golden/maps.bin", "wb").write(maps)

# cross-validate both against protoc's decoder
for name, data in [("entry_push", push), ("maps", maps)]:
    p = subprocess.run(
        ["protoc", "--decode=walgit.v1.LogEntry", "-I/usr/include", "-I.", "wal.proto"],
        input=data, capture_output=True)
    if p.returncode != 0:
        sys.exit(f"protoc rejected {name}: {p.stderr.decode()}")
    print(name, "accepted by protoc,", len(data), "bytes")
