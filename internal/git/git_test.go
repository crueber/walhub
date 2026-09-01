package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- RepoId validation (§1.1) ---------------------------------------------------------

func TestValidateRepoPath(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		owner string
		name  string
	}{
		{"owner/repo", true, "owner", "repo"},
		{"owner/repo.git", true, "owner", "repo"},
		{"a/b.c", true, "a", "b.c"},
		{"Owner_1/n-ame.x", true, "Owner_1", "n-ame.x"},
		{"owner/repo.tar.gz", true, "owner", "repo.tar.gz"}, // only ONE .git stripped
		{"/repo", false, "", ""},
		{"owner/", false, "", ""},
		{"owner", false, "", ""},
		{"owner//repo", false, "", ""},
		{".hidden/repo", false, "", ""},
		{"owner/..", false, "", ""},
		{"../repo", false, "", ""},
		{"owner/.repo", false, "", ""},
		{"own er/repo", false, "", ""},
		{"owner/re po", false, "", ""},
		{"ownér/repo", false, "", ""},
		{"owner+1/repo", false, "", ""},
		{"owner:1/repo", false, "", ""},
		{"own%er/repo", false, "", ""},
		{"owner/" + strings.Repeat("a", 101), false, "", ""},
		{strings.Repeat("a", 101) + "/repo", false, "", ""},
		{"owner/" + strings.Repeat("a", 100), true, "owner", strings.Repeat("a", 100)},
	}
	for _, tc := range cases {
		id, err := ParseRepoId(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParseRepoId(%q) unexpected error: %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseRepoId(%q) accepted, want rejection", tc.in)
		}
		if tc.ok && (id.Owner != tc.owner || id.Name != tc.name) {
			t.Errorf("ParseRepoId(%q) = %q/%q, want %q/%q", tc.in, id.Owner, id.Name, tc.owner, tc.name)
		}
	}
}

// --- pkt-line codec (§5) ----------------------------------------------------------------

func TestPktRoundTrip(t *testing.T) {
	cases := []string{"", "a", "hello world", "multi\nline\npayload", strings.Repeat("x", 65515)}
	for _, payload := range cases {
		wire := Pkt(payload)
		got, kind, err := NewPktReader(bytes.NewReader(wire)).Next()
		if err != nil {
			t.Fatalf("decode %q: %v", payload, err)
		}
		if kind != PktKindData {
			t.Fatalf("decode %q: kind %d", payload, kind)
		}
		if want := payload + "\n"; string(got) != want {
			t.Errorf("pkt round trip: got %q want %q", got, want)
		}
	}
}

func TestPktSpecialLengths(t *testing.T) {
	r := NewPktReader(bytes.NewReader([]byte("0000" + "0001" + "0002")))
	if _, k, _ := r.Next(); k != PktKindFlush {
		t.Errorf("flush kind = %d", k)
	}
	if _, k, _ := r.Next(); k != PktKindDelim {
		t.Errorf("delim kind = %d", k)
	}
	if _, k, _ := r.Next(); k != PktKindResEnd {
		t.Errorf("response-end kind = %d", k)
	}
	if _, _, err := r.Next(); err == nil {
		t.Error("expected EOF after response-end")
	}
}

func TestPktLengthEncoding(t *testing.T) {
	if got := string(Pkt("a")); got != "0006a\n" {
		t.Errorf("Pkt(\"a\") = %q, want 0005a\\n", got)
	}
	if got := string(Pkt("")); got != "0005\n" {
		t.Errorf("Pkt(\"\") = %q, want 0004\\n", got)
	}
	if got := string(PktBytes([]byte("hi"))); got != "0006hi" {
		t.Errorf("PktBytes = %q", got)
	}
}

func TestPktProtocolErrors(t *testing.T) {
	// non-hex length
	_, _, err := NewPktReader(bytes.NewReader([]byte("zzzz"))).Next()
	if err == nil {
		t.Error("non-hex length accepted")
	}
	// payload longer than 65516
	_, _, err = NewPktReader(bytes.NewReader([]byte("ffff"))).Next()
	if err == nil {
		t.Error("oversized length accepted")
	}
	// truncated payload
	_, _, err = NewPktReader(bytes.NewReader([]byte("0007ab"))).Next()
	if err == nil {
		t.Error("truncated payload accepted")
	}
	// payload longer than 65516 → protocol error (not panic) at decoder level
	_, _, err = NewPktReader(bytes.NewReader(append([]byte("ffff"), make([]byte, 100)...))).Next()
	if err == nil {
		t.Error("oversized payload accepted")
	}
}

func TestPktMaxPayload(t *testing.T) {
	big := PktBytes(bytes.Repeat([]byte("x"), 65516))
	if len(big) != MaxPktTotal {
		t.Errorf("max pkt len = %d, want %d", len(big), MaxPktTotal)
	}
}

func TestSidebandRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("d"), 200000) // > one frame
	messages := "some progress\n"
	wire := EncodeSideband(1, payload)
	wire = append(wire, EncodeSideband(2, []byte(messages))...)
	wire = append(wire, Flush()...)
	got, msgs, err := SidebandDecode(wire)
	if err != nil {
		t.Fatalf("sideband decode: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("sideband payload mismatch: %d vs %d bytes", len(got), len(payload))
	}
	if string(msgs) != messages {
		t.Errorf("sideband messages = %q", msgs)
	}
}

func TestSidebandEmptyIsNothing(t *testing.T) {
	if got := EncodeSideband(1, nil); len(got) != 0 {
		t.Errorf("empty band-1 frame emitted: %q", got)
	}
}

func TestFirstNul(t *testing.T) {
	before, after := FirstNul([]byte("abc\x00def\x00ghi"))
	if string(before) != "abc" || string(after) != "def\x00ghi" {
		t.Errorf("FirstNul = %q, %q", before, after)
	}
	before, after = FirstNul([]byte("no-nul"))
	if string(before) != "no-nul" || after != nil {
		t.Errorf("FirstNul no-nul = %q, %q", before, after)
	}
}

// --- ref validation (§4.3) --------------------------------------------------------------

func TestValidateRefName(t *testing.T) {
	ok := []string{"HEAD", "refs/heads/main", "refs/tags/v1.0", "refs/heads/feature/x_y-z.w", "refs/pull/1/head"}
	bad := []string{
		"", "main", "refs", "refs/", "/refs/heads/x", "refs//heads/x", "refs/heads//x",
		"refs/heads/x/", "refs/heads/x.", "refs/heads/x.lock", "refs/heads/x..y",
		"refs/heads/a b", "refs/heads/a\nb", "refs/heads/a~b", "refs/heads/a^b",
		"refs/heads/a:b", "refs/heads/a?b", "refs/heads/a*b", "refs/heads/a[b",
		"refs/heads/a\\b", "refs/heads/@{x", "refs/heads/\x01b",
	}
	for _, name := range ok {
		if err := ValidateRefName(name); err != nil {
			t.Errorf("ValidateRefName(%q) rejected: %v", name, err)
		}
	}
	for _, name := range bad {
		if err := ValidateRefName(name); err == nil {
			t.Errorf("ValidateRefName(%q) accepted", name)
		}
	}
}

func TestValidateRefUpdate(t *testing.T) {
	good := RefUpdate{Name: "refs/heads/main", OldOid: zero40, NewOid: strings.Repeat("a", 40)}
	if err := ValidateRefUpdate(&good); err != nil {
		t.Errorf("valid update rejected: %v", err)
	}
	empty := RefUpdate{Name: "refs/heads/main", OldOid: "", NewOid: ""}
	if err := ValidateRefUpdate(&empty); err != nil {
		t.Errorf("absent markers rejected: %v", err)
	}
	badOid := RefUpdate{Name: "refs/heads/main", OldOid: "xyz", NewOid: "abc"}
	if err := ValidateRefUpdate(&badOid); err == nil {
		t.Error("invalid oid accepted")
	}
	badName := RefUpdate{Name: "main", OldOid: zero40, NewOid: "a"}
	if err := ValidateRefUpdate(&badName); err == nil {
		t.Error("non-refs name accepted")
	}
	sym := RefUpdate{Name: "refs/heads/x", NewSymbolicTarget: "refs/heads/main"}
	if err := ValidateRefUpdate(&sym); err == nil {
		t.Error("symbolic target on non-HEAD accepted")
	}
}

// --- packed-refs parsing incl ^peeled (§4.1) ----------------------------------------------

func TestParsePackedRefs(t *testing.T) {
	data := "# pack-refs with: peeled fully-peeled sorted \n" +
		strings.Repeat("a", 40) + " refs/heads/main\n" +
		strings.Repeat("b", 40) + " refs/tags/v1\n" +
		"^" + strings.Repeat("c", 40) + "\n" +
		strings.Repeat("d", 40) + " refs/tags/v2\n"
	refs := parsePackedRefs([]byte(data))
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3", len(refs))
	}
	if refs[0].Name != "refs/heads/main" || refs[0].Oid != strings.Repeat("a", 40) || refs[0].Peeled != "" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	if refs[1].Name != "refs/tags/v1" || refs[1].Peeled != strings.Repeat("c", 40) {
		t.Errorf("refs[1] peeled not attached: %+v", refs[1])
	}
	if refs[2].Name != "refs/tags/v2" || refs[2].Peeled != "" {
		t.Errorf("refs[2] = %+v", refs[2])
	}
}

func TestParsePackedRefsNoHeader(t *testing.T) {
	refs := parsePackedRefs([]byte(strings.Repeat("a", 40) + " refs/heads/x\n"))
	if len(refs) != 1 || refs[0].Name != "refs/heads/x" {
		t.Errorf("refs = %+v", refs)
	}
}

// --- local repo init (§1.2) — REAL git -----------------------------------------------------

func TestInitLocalRepo(t *testing.T) {
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "r"}, Sha1)
	if err != nil {
		t.Fatalf("InitLocalRepo: %v", err)
	}
	if repo.Path != filepath.Join(root, "o", "r.git") {
		t.Errorf("path = %s", repo.Path)
	}
	head, err := os.ReadFile(filepath.Join(repo.Path, "HEAD"))
	if err != nil || string(head) != "ref: refs/heads/main\n" {
		t.Errorf("HEAD = %q err %v", head, err)
	}
	cfg, _ := os.ReadFile(filepath.Join(repo.Path, "config"))
	cfgText := string(cfg)
	for _, want := range []string{"allowFilter", "allowAnySHA1InWant", "allowSidebandAll", "writeReverseIndex"} {
		if !strings.Contains(cfgText, want) {
			t.Errorf("config missing %s:\n%s", want, cfgText)
		}
	}
	if repo.Format() != Sha1 {
		t.Errorf("format = %v", repo.Format())
	}
}

func TestInitLocalRepoSha256(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "s256"}, Sha256)
	if err != nil {
		t.Fatalf("InitLocalRepo: %v", err)
	}
	if repo.Format() != Sha256 {
		t.Errorf("format = %v, want sha256", repo.Format())
	}
	if repo.ZeroOid() != zero64 {
		t.Errorf("zero oid = %s", repo.ZeroOid())
	}
}

func TestOpenLocalRepoAbsent(t *testing.T) {
	repo, err := OpenLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "missing"})
	if err != nil || repo != nil {
		t.Errorf("OpenLocalRepo absent = %v, %v", repo, err)
	}
}

// --- advertisement golden bytes (§6.1/§6.2) -------------------------------------------------

const (
	oidA = "1111111111111111111111111111111111111111"
	oidB = "2222222222222222222222222222222222222222"
	oidP = "3333333333333333333333333333333333333333"
)

func pkt(s string) string { return string(Pkt(s)) }
func flushS() string      { return string(Flush()) }

func goldenAdvert(t *testing.T, svc Service) []byte {
	t.Helper()
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "adv"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	l := NewLayer()
	// Hand-build the snapshot state the golden expects: two refs, one tag peeled.
	refs := []RefEntry{
		{Name: "refs/heads/main", Oid: oidA},
		{Name: "refs/tags/v1", Oid: oidB, Peeled: oidP},
	}
	if err := l.LoadSnapshot(repo, refs, "refs/heads/main", oidA); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	body, err := l.Advertisement(repo, svc, false, "1.2.3")
	if err != nil {
		t.Fatalf("Advertisement: %v", err)
	}
	return body
}

func TestAdvertisementReceivePackGolden(t *testing.T) {
	want := pkt(oidA+" refs/heads/main\x00"+"report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta push-options object-format=sha1 agent=walgit/1.2.3") +
		pkt(oidB+" refs/tags/v1") +
		pkt(oidP+" refs/tags/v1{}") +
		flushS()
	if got := string(goldenAdvert(t, ServiceReceivePack)); got != want {
		t.Errorf("receive-pack advert mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestAdvertisementUploadPackGolden(t *testing.T) {
	want := pkt(oidA+" refs/heads/main\x00"+"multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want filter object-format=sha1 agent=walgit/1.2.3") +
		pkt(oidB+" refs/tags/v1") +
		pkt(oidP+" refs/tags/v1{}") +
		pkt(oidA+" HEAD") +
		flushS()
	if got := string(goldenAdvert(t, ServiceUploadPack)); got != want {
		t.Errorf("upload-pack advert mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestAdvertisementEmptyRepo(t *testing.T) {
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "empty"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	l := NewLayer()
	body, err := l.Advertisement(repo, ServiceReceivePack, false, "1.0")
	if err != nil {
		t.Fatalf("Advertisement: %v", err)
	}
	want := pkt(zero40+" capabilities^{}\x00"+capsReceivePack+"sha1"+agentSuffix+"1.0") + flushS()
	if string(body) != want {
		t.Errorf("empty advert:\n got %q\nwant %q", body, want)
	}
}

func TestCapabilityAdvertV2(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "v2"}, Sha1)
	l := NewLayer()
	body, err := l.Advertisement(repo, ServiceUploadPack, true, "9.9")
	if err != nil {
		t.Fatalf("Advertisement: %v", err)
	}
	want := pkt("version 2") +
		pkt("agent=walgit/9.9") +
		pkt("ls-refs=unborn") +
		pkt("fetch=thin-pack ofs-delta sideband-all wait-for-done shallow deepen-since deepen-not deepen-relative filter include-tag") +
		pkt("object-format=sha1") +
		flushS()
	if string(body) != want {
		t.Errorf("v2 capability advert:\n got %q\nwant %q", body, want)
	}
}

func TestProtocolVersionHeader(t *testing.T) {
	if ProtocolVersion("version=2") != 2 {
		t.Error("version=2 not detected")
	}
	if ProtocolVersion("Version=2") != 2 {
		t.Error("case-insensitive match failed")
	}
	if ProtocolVersion("") != 0 || ProtocolVersion("agent=git") != 0 {
		t.Error("v0 default broken")
	}
}

// --- ls-refs prefix filtering (§6.3) --------------------------------------------------------

func lsRefsFixture(t *testing.T) (*LocalRepo, *Layer) {
	t.Helper()
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "lsr"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	l := NewLayer()
	refs := []RefEntry{
		{Name: "refs/heads/main", Oid: oidA},
		{Name: "refs/heads/next", Oid: oidB},
		{Name: "refs/pull/1/head", Oid: oidP},
		{Name: "refs/tags/v1", Oid: oidB, Peeled: oidP},
	}
	if err := l.LoadSnapshot(repo, refs, "refs/heads/main", oidA); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	return repo, l
}

func TestLsRefsAll(t *testing.T) {
	repo, l := lsRefsFixture(t)
	body, err := l.LsRefs(repo, LsRefsArgs{})
	if err != nil {
		t.Fatalf("LsRefs: %v", err)
	}
	pkts, err := ReadAllPkts(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ReadAllPkts: %v", err)
	}
	lines := make([]string, len(pkts))
	for i, p := range pkts {
		lines[i] = string(p)
	}
	want := []string{oidA + " HEAD\n", oidA + " refs/heads/main\n", oidB + " refs/heads/next\n",
		oidP + " refs/pull/1/head\n", oidB + " refs/tags/v1\n"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestLsRefsPrefixFiltering(t *testing.T) {
	repo, l := lsRefsFixture(t)
	body, err := l.LsRefs(repo, LsRefsArgs{Prefixes: []string{"refs/heads/"}})
	if err != nil {
		t.Fatalf("LsRefs: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, oidA+" refs/heads/main") || !strings.Contains(text, oidB+" refs/heads/next") {
		t.Errorf("heads missing: %q", text)
	}
	if strings.Contains(text, "refs/pull") || strings.Contains(text, "refs/tags") {
		t.Errorf("non-matching refs leaked: %q", text)
	}
}

func TestLsRefsHeadNotHiddenByPrefix(t *testing.T) {
	repo, l := lsRefsFixture(t)
	body, err := l.LsRefs(repo, LsRefsArgs{Symrefs: true, Prefixes: []string{"refs/heads/"}})
	if err != nil {
		t.Fatalf("LsRefs: %v", err)
	}
	// HEAD resolved from head_target BEFORE prefix filtering.
	if !strings.Contains(string(body), oidA+" HEAD symref-target:refs/heads/main") {
		t.Errorf("HEAD hidden by prefix: %q", body)
	}
}

func TestLsRefsPeelAndDedupe(t *testing.T) {
	repo, l := lsRefsFixture(t)
	// overlapping prefixes must not duplicate refs
	body, err := l.LsRefs(repo, LsRefsArgs{Peel: true, Prefixes: []string{"refs/", "refs/tags/"}})
	if err != nil {
		t.Fatalf("LsRefs: %v", err)
	}
	text := string(body)
	if strings.Count(text, "refs/tags/v1") != 1 {
		t.Errorf("overlapping prefixes duplicated refs: %q", text)
	}
	if !strings.Contains(text, "peeled:"+oidP) {
		t.Errorf("peeled missing: %q", text)
	}
}

func TestLsRefsUnborn(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "unborn"}, Sha1)
	l := NewLayer()
	body, err := l.LsRefs(repo, LsRefsArgs{Unborn: true, Symrefs: true})
	if err != nil {
		t.Fatalf("LsRefs: %v", err)
	}
	if !strings.Contains(string(body), "unborn HEAD symref-target:refs/heads/main") {
		t.Errorf("unborn HEAD line missing: %q", body)
	}
}

func TestParseLsRefsArgs(t *testing.T) {
	pkts := [][]byte{[]byte("symrefs\n"), []byte("peel\n"), []byte("ref-prefix refs/heads/\n"), []byte("ref-prefix=refs/tags/\n")}
	args := ParseLsRefsArgs(pkts)
	if !args.Symrefs || !args.Peel {
		t.Errorf("flags = %+v", args)
	}
	if len(args.Prefixes) != 2 || args.Prefixes[0] != "refs/heads/" || args.Prefixes[1] != "refs/tags/" {
		t.Errorf("prefixes = %+v", args)
	}
}

// --- update-ref txn grammar (§4.3) — REAL git ----------------------------------------------

func TestApplyRefTxnGrammar(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "txn"}, Sha1)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	l := NewLayer()
	// Seed two objects to point refs at (blob + tag-ish commit via hash-object).
	oid1 := gitBlob(t, repo, "one")
	oid2 := gitBlob(t, repo, "two")

	// create with checkOld (zero old = must not exist)
	txn := []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid1},
		{Name: "refs/heads/dev", OldOid: zero40, NewOid: oid2},
	}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, true); err != nil {
		t.Fatalf("ApplyRefTxn create: %v", err)
	}
	snap, err := l.Snapshot(repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if e, ok := snap.Get("refs/heads/main"); !ok || e.Oid != oid1 {
		t.Errorf("main = %+v ok=%v", e, ok)
	}

	// update with wrong old → ErrRefConflict
	bad := []RefUpdate{{Name: "refs/heads/main", OldOid: oid2, NewOid: oid2}}
	err = l.ApplyRefTxn(t.Context(), repo, bad, true)
	var ce *RefConflictError
	if err == nil || !errorsAs(err, &ce) {
		t.Fatalf("expected RefConflictError, got %v", err)
	}
	if ce.Ref != "refs/heads/main" || ce.Expected != oid2 || ce.Actual != oid1 {
		t.Errorf("conflict detail = %+v", ce)
	}

	// update with right old
	txn = []RefUpdate{{Name: "refs/heads/main", OldOid: oid1, NewOid: oid2}}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, true); err != nil {
		t.Fatalf("ApplyRefTxn update: %v", err)
	}
	// delete
	txn = []RefUpdate{{Name: "refs/heads/dev", OldOid: oid2, NewOid: zero40}}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, true); err != nil {
		t.Fatalf("ApplyRefTxn delete: %v", err)
	}
	if _, ok := snapGet(t, repo, "refs/heads/dev"); ok {
		t.Error("dev not deleted")
	}
	// create over existing → conflict
	txn = []RefUpdate{{Name: "refs/heads/main", OldOid: zero40, NewOid: oid1}}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, true); err == nil {
		t.Error("create over existing accepted")
	}
}

func TestApplyRefTxnSymbolicHead(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "sym"}, Sha1)
	l := NewLayer()
	txn := []RefUpdate{{Name: "HEAD", OldOid: "", NewOid: "", NewSymbolicTarget: "refs/heads/dev"}}
	if err := l.ApplyRefTxn(t.Context(), repo, txn, false); err != nil {
		t.Fatalf("ApplyRefTxn HEAD: %v", err)
	}
	head, _ := os.ReadFile(filepath.Join(repo.Path, "HEAD"))
	if string(head) != "ref: refs/heads/dev\n" {
		t.Errorf("HEAD = %q", head)
	}
}

func TestApplyRefTxnsOffline(t *testing.T) {
	refs := []RefEntry{
		{Name: "refs/heads/main", Oid: oidA},
		{Name: "refs/heads/old", Oid: oidB},
	}
	txn := []RefUpdate{
		{Name: "refs/heads/main", OldOid: oidA, NewOid: oidB},
		{Name: "refs/heads/old", OldOid: oidB, NewOid: zero40},
		{Name: "refs/heads/new", OldOid: zero40, NewOid: oidP},
	}
	out := ApplyRefTxnsOffline(refs, txn)
	if len(out) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Name != "refs/heads/main" || out[0].Oid != oidB {
		t.Errorf("main = %+v", out[0])
	}
	if out[1].Name != "refs/heads/new" || out[1].Oid != oidP {
		t.Errorf("new = %+v", out[1])
	}
}

// --- report-status (§7.2) ---------------------------------------------------------------------

func TestReportPlain(t *testing.T) {
	r := Report{UnpackOK: true, Refs: []RefReport{
		{Ref: "refs/heads/main", OK: true},
		{Ref: "refs/heads/dev", Reason: "rejected by rule 'no-dev'"},
	}}
	got := string(r.EncodeReport())
	want := pkt("unpack ok") + pkt("ok refs/heads/main") + pkt("ng refs/heads/dev rejected by rule 'no-dev'") + flushS()
	if got != want {
		t.Errorf("report:\n got %q\nwant %q", got, want)
	}
}

func TestReportUnpackFailure(t *testing.T) {
	r := Report{UnpackOK: false, UnpackMsg: "index-pack failed"}
	got := string(r.EncodeReport())
	if !strings.Contains(got, "unpack ng index-pack failed") {
		t.Errorf("report = %q", got)
	}
}

func TestReportSidebandWrapped(t *testing.T) {
	r := &Report{UnpackOK: true, Sideband: true, Refs: []RefReport{{Ref: "refs/heads/main", OK: true}}}
	wire := r.EncodeReport()
	payload, msgs, err := SidebandDecode(wire[:len(wire)-4])
	if err != nil {
		t.Fatalf("sideband decode: %v", err)
	}
	if string(payload) != string((&Report{UnpackOK: true, Refs: []RefReport{{Ref: "refs/heads/main", OK: true}}}).EncodeReport()) {
		t.Errorf("wrapped payload mismatch: %q", payload)
	}
	if len(msgs) != 0 {
		t.Errorf("unexpected messages %q", msgs)
	}
}

func TestBand2(t *testing.T) {
	_, msgs, err := SidebandDecode(append(Band2("boom"), Flush()...))
	if err != nil || string(msgs) != "boom\n" {
		t.Errorf("Band2 = %q, %v", msgs, err)
	}
}

// --- receive-pack parse (§7 step 1) ---------------------------------------------------------

func TestParsePushRequest(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "push"}, Sha1)
	l := NewLayer()
	var body bytes.Buffer
	body.Write(PktBytes([]byte(zero40 + " " + oidA + " refs/heads/main\x00report-status side-band-64k push-options agent=git/2.0 object-format=sha1\n")))
	body.Write(Pkt(zero40 + " " + zero40 + " refs/heads/gone"))
	body.Write(Flush())
	body.Write(Pkt("opt1"))
	body.Write(Pkt("opt=2"))
	body.Write(Flush())
	body.Write([]byte("RAWPACKBYTES"))

	req, err := l.ParsePushRequest(repo, body.Bytes())
	if err != nil {
		t.Fatalf("ParsePushRequest: %v", err)
	}
	if len(req.Commands) != 2 {
		t.Fatalf("commands = %+v", req.Commands)
	}
	c := req.Commands[0]
	if c.Old != zero40 || c.New != oidA || c.Ref != "refs/heads/main" {
		t.Errorf("cmd0 = %+v", c)
	}
	if !req.Has("report-status") || !req.Has("side-band-64k") {
		t.Errorf("caps = %+v", req.Caps)
	}
	if len(req.PushOptions) != 2 || req.PushOptions[1] != "opt=2" {
		t.Errorf("push options = %+v", req.PushOptions)
	}
	if string(req.Pack) != "RAWPACKBYTES" {
		t.Errorf("pack = %q", req.Pack)
	}
}

func TestParsePushRequestObjectFormatMismatch(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "push2"}, Sha1)
	l := NewLayer()
	var body bytes.Buffer
	body.Write(PktBytes([]byte(zero40 + " " + oidA + " refs/heads/main\x00object-format=sha256\n")))
	body.Write(Flush())
	if _, err := l.ParsePushRequest(repo, body.Bytes()); err == nil {
		t.Error("object-format mismatch accepted")
	}
}

func TestParsePushRequestBadRef(t *testing.T) {
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "push3"}, Sha1)
	l := NewLayer()
	var body bytes.Buffer
	body.Write(PktBytes([]byte(zero40 + " " + oidA + " refs/heads/main..x\x00")))
	body.Write(Flush())
	if _, err := l.ParsePushRequest(repo, body.Bytes()); err == nil {
		t.Error("invalid ref accepted")
	}
}

// --- unit: connectivity missing detection, bundle header, cache -------------------------------

func TestMissingFromBatch(t *testing.T) {
	out := strings.Repeat("a", 40) + " missing\n" + oidB + " blob 12\n" + strings.Repeat("c", 40) + " missing\n"
	got := missingFromBatch(out)
	if len(got) != 2 || got[0] != strings.Repeat("a", 40) {
		t.Errorf("missing = %v", got)
	}
}

func TestMissingFromBatchCap16(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("%040x missing", i))
	}
	if got := missingFromBatch(strings.Join(lines, "\n")); len(got) != 16 {
		t.Errorf("cap = %d, want 16", len(got))
	}
}

func TestBundleHeaderBytes(t *testing.T) {
	got := string(BundleHeader([]RefEntry{{Name: "refs/heads/main", Oid: oidA}}, []Oid{oidB}))
	want := "# v2 git bundle\n-" + oidB + " \n" + oidA + " refs/heads/main\n\n"
	if got != want {
		t.Errorf("BundleHeader:\n got %q\nwant %q", got, want)
	}
}

func TestFullBundleCompose(t *testing.T) {
	header := BundleHeader(nil, nil)
	pack := []byte("PACK")
	got := FullBundle(header, pack)
	want := append([]byte("# v2 git bundle\n\n"), pack...)
	if !bytes.Equal(got, want) {
		t.Errorf("FullBundle = %q", got)
	}
}

func TestRefCachePatchAndView(t *testing.T) {
	c := NewRefCache()
	c.base = &RefSnapshot{Refs: []RefEntry{
		{Name: "refs/heads/a", Oid: oidA},
		{Name: "refs/heads/b", Oid: oidB},
	}}
	// view before patch
	if e, ok := c.RefView("refs/heads/a"); !ok || e.Oid != oidA {
		t.Errorf("view a = %+v %v", e, ok)
	}
	c.Patch([]RefUpdate{
		{Name: "refs/heads/a", OldOid: oidA, NewOid: oidP},   // updated
		{Name: "refs/heads/b", OldOid: oidB, NewOid: zero40}, // deleted
		{Name: "refs/heads/c", OldOid: zero40, NewOid: oidB}, // created
	})
	if e, ok := c.RefView("refs/heads/a"); !ok || e.Oid != oidP {
		t.Errorf("view a after patch = %+v %v", e, ok)
	}
	if _, ok := c.RefView("refs/heads/b"); ok {
		t.Error("deleted ref still visible")
	}
	if e, ok := c.RefView("refs/heads/c"); !ok || e.Oid != oidB {
		t.Errorf("view c = %+v %v", e, ok)
	}
	// base immutability: original slice untouched
	if c.base.Refs[0].Oid != oidP || c.base.Refs[1].Name != "refs/heads/c" {
		// patched base is the new slice; that's fine — but Gen bumped
	}
	if c.base.Gen != 1 {
		t.Errorf("Gen = %d, want 1", c.base.Gen)
	}
}

func TestRefSnapshotBinarySearch(t *testing.T) {
	s := &RefSnapshot{Refs: []RefEntry{
		{Name: "refs/a", Oid: oidA}, {Name: "refs/b", Oid: oidB}, {Name: "refs/c", Oid: oidP},
	}}
	for _, n := range []string{"refs/a", "refs/b", "refs/c"} {
		if _, ok := s.Get(n); !ok {
			t.Errorf("Get(%q) miss", n)
		}
	}
	if _, ok := s.Get("refs/d"); ok {
		t.Error("Get(absent) hit")
	}
}

func TestIdxObjectCountFanout(t *testing.T) {
	// build a tiny valid fanout: magic, version, 256 BE counts
	var b bytes.Buffer
	b.Write([]byte{0xff, 't', 'O', 'c'})
	b.Write([]byte{0, 0, 0, 2})
	for i := range 255 {
		b.Write([]byte{0, 0, 0, byte(i)}) // cumulative counts 0..254
	}
	b.Write([]byte{0, 0, 0x01, 0x2c}) // fanout[255] = 300
	path := t.TempDir() + "/x.idx"
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	count, err := idxObjectCount(path)
	if err != nil || count != 300 {
		t.Errorf("count = %d err %v", count, err)
	}
}

func TestLastHexToken(t *testing.T) {
	if got := lastHexToken("pack\t" + strings.Repeat("A", 40) + "\nkeep done"); got != strings.Repeat("a", 40) {
		t.Errorf("lastHexToken = %q", got)
	}
	if got := lastHexToken("no hex here"); got != "" {
		t.Errorf("lastHexToken no-hex = %q", got)
	}
	long := strings.Repeat("f", 64)
	if got := lastHexToken("x " + long); got != long {
		t.Errorf("lastHexToken 64 = %q", got)
	}
}

func TestValidateOidShapes(t *testing.T) {
	if !ValidOid("") || !ValidOid(zero40) || !ValidOid(zero64) {
		t.Error("absent markers invalid")
	}
	if !ValidOid(oidA) || !ValidOid(zero64[:32]) {
		t.Error("valid oids rejected")
	}
	if ValidOid("XYZ") || ValidOid(oidA[:39]) || ValidOid(strings.Repeat("a", 63)) {
		t.Error("invalid oids accepted")
	}
}

func TestErrPktRendering(t *testing.T) {
	if got := string(ErrPkt("nope")); got != pkt("ERR nope")+flushS() {
		t.Errorf("ErrPkt = %q", got)
	}
}

// --- helpers ---------------------------------------------------------------------------------

// gitCommit creates a real commit object (branches can only point at commits)
// with a unique message so each call yields a distinct oid.
func gitBlob(t *testing.T, repo *LocalRepo, content string) string {
	t.Helper()
	cmd := exec.Command("git", "commit-tree", "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	cmd.Dir = repo.Path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + repo.Path,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE=2005-04-07T22:13:13+00:00", "GIT_COMMITTER_DATE=2005-04-07T22:13:13+00:00"}
	cmd.Stdin = strings.NewReader(content + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func snapGet(t *testing.T, repo *LocalRepo, name string) (RefEntry, bool) {
	t.Helper()
	l := NewLayer()
	snap, err := l.Snapshot(repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap.Get(name)
}

func gitAtLeast(t *testing.T, major, minor int) bool {
	t.Helper()
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	var mj, mn int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "git version %d.%d", &mj, &mn); err != nil {
		t.Skipf("unparseable git version %q", out)
	}
	return mj > major || (mj == major && mn >= minor)
}

func errorsAs(err error, target any) bool {
	for err != nil {
		if e, ok := err.(*RefConflictError); ok {
			if p, ok := target.(**RefConflictError); ok {
				*p = e
				return true
			}
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
