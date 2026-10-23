package buildstate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Independent OS processes share one PostgreSQL primary (TX1-07 (1), N1 startup counterexample).
// Each process has its own connection pool; goroutines in one process would not count.

const (
	pgHelperDSNEnv     = "NV_BUILDSTATE_HELPER_PGDSN"
	pgHelperBarrierEnv = "NV_BUILDSTATE_HELPER_BARRIER"
	pgHelperRoleEnv    = "NV_BUILDSTATE_HELPER_ROLE"
	pgHelperModeEnv    = "NV_BUILDSTATE_HELPER_MODE"
	pgHelperPrefix     = "NVBS-RESULT"
	pgHelperRounds     = 20
)

// TestPostgresHelperProcess is the child side of the multi-process tests below.
func TestPostgresHelperProcess(t *testing.T) {
	dsn, barrier, role, mode := os.Getenv(pgHelperDSNEnv), os.Getenv(pgHelperBarrierEnv),
		os.Getenv(pgHelperRoleEnv), os.Getenv(pgHelperModeEnv)
	if dsn == "" {
		t.Skip("helper process for the PostgreSQL multi-process tests")
	}
	ctx := context.Background()
	s, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("helper %s open: %v", role, err)
	}
	defer func() { _ = s.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil { //nolint:gosec // barrier is the parent test's t.TempDir() file
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper %s: start barrier never opened", role)
		}
		time.Sleep(time.Millisecond)
	}
	switch mode {
	case "contend":
		for i := range pgHelperRounds {
			_, f, created, err := s.CreateOrGet(ctx, "shared-"+strconv.Itoa(i), "sha256:spec", role, liveTTL)
			printHelper(role, i, "create", fmt.Sprintf("created=%v fenced=%v err=%v", created, f.Owner == role, err))
			_, _, err = s.Reclaim(ctx, "expired-"+strconv.Itoa(i), role, liveTTL)
			printHelper(role, i, "reclaim", reclaimOutcome(err))
		}
	case "recover":
		n, err := s.RecoverExpired(ctx, role)
		printHelper(role, 0, "recover", fmt.Sprintf("count=%d err=%v", n, err))
	default:
		t.Fatalf("helper %s: unknown mode %q", role, mode)
	}
}

func reclaimOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrLeaseHeld):
		return "held"
	default:
		return "err:" + err.Error()
	}
}

func printHelper(role string, round int, op, result string) {
	_, _ = os.Stdout.WriteString(pgHelperPrefix + " " + role + " " + strconv.Itoa(round) + " " + op + " " + result + "\n")
}

type pgHelperResult struct {
	role, op, result string
	round            int
}

// runHelpers starts one helper process per role, opens the start barrier, waits for all of
// them and returns their result lines.
func runHelpers(t *testing.T, dsn, mode string, roles ...string) []pgHelperResult {
	t.Helper()
	barrier := filepath.Join(t.TempDir(), "start")
	cmds := map[string]*exec.Cmd{}
	outs := map[string]*bytes.Buffer{}
	for _, role := range roles {
		cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestPostgresHelperProcess$", "-test.count=1", "-test.v") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), pgHelperDSNEnv+"="+dsn, pgHelperBarrierEnv+"="+barrier,
			pgHelperRoleEnv+"="+role, pgHelperModeEnv+"="+mode)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %s: %v", role, err)
		}
		cmds[role], outs[role] = cmd, &buf
	}
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatalf("open start barrier: %v", err)
	}
	var results []pgHelperResult
	for role, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %s failed: %v\n%s", role, err, outs[role].String())
		}
		sc := bufio.NewScanner(bytes.NewReader(outs[role].Bytes()))
		for sc.Scan() {
			fields := strings.SplitN(sc.Text(), " ", 5)
			if len(fields) != 5 || fields[0] != pgHelperPrefix {
				continue
			}
			round, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatalf("bad helper line %q", sc.Text())
			}
			results = append(results, pgHelperResult{role: fields[1], round: round, op: fields[3], result: fields[4]})
		}
	}
	return results
}

// TX1-07 (1): two processes create the same build IDs and reclaim the same expired builds at
// once. Each build has exactly one creator, which alone gets the fence, and each expired build
// exactly one reclaimer, whose generation the store then holds.
func TestPostgres_TwoProcessCreateAndReclaim(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns helper processes")
	}
	dsn := pgSchemaDSN(t)
	seed := openPG(t, dsn)
	for i := range pgHelperRounds {
		_, f := mustCreate(t, seed, "expired-"+strconv.Itoa(i), "seed")
		if _, err := seed.RenewLease(context.Background(), "expired-"+strconv.Itoa(i), f, time.Microsecond); err != nil {
			t.Fatalf("expire seed lease: %v", err)
		}
	}
	time.Sleep(10 * time.Millisecond)

	results := runHelpers(t, dsn, "contend", "a", "b")
	if len(results) != 2*2*pgHelperRounds {
		t.Fatalf("helper results = %d, want %d: %+v", len(results), 2*2*pgHelperRounds, results)
	}
	verifyContention(t, seed, results)
	if n := rawCount(t, dsn, `SELECT count(*) FROM nv_build_state`); n != 2*pgHelperRounds {
		t.Fatalf("rows = %d, want %d", n, 2*pgHelperRounds)
	}
}

// verifyContention checks that each shared build has exactly one creator, which alone got the
// fence and owns the build, and each expired build exactly one reclaimer, which holds generation 2.
func verifyContention(t *testing.T, s *PostgresStore, results []pgHelperResult) {
	t.Helper()
	creators := map[int][]string{}
	reclaimers := map[int][]string{}
	for _, r := range results {
		switch {
		case r.op == "create" && r.result == "created=true fenced=true err=<nil>":
			creators[r.round] = append(creators[r.round], r.role)
		case r.op == "create" && r.result == "created=false fenced=false err=<nil>":
		case r.op == "reclaim" && r.result == "ok":
			reclaimers[r.round] = append(reclaimers[r.round], r.role)
		case r.op == "reclaim" && r.result == "held":
		default:
			t.Fatalf("unexpected helper result %+v", r)
		}
	}
	for i := range pgHelperRounds {
		if len(creators[i]) != 1 || len(reclaimers[i]) != 1 {
			t.Fatalf("round %d: creators %v reclaimers %v, want exactly one each", i, creators[i], reclaimers[i])
		}
		rec, f := mustGet(t, s, "shared-"+strconv.Itoa(i))
		if f != (Fence{Owner: creators[i][0], Generation: 1, Version: 1}) || rec.Status != StatusRequested {
			t.Fatalf("round %d shared: %+v %+v, creator %s", i, rec, f, creators[i][0])
		}
		_, f = mustGet(t, s, "expired-"+strconv.Itoa(i))
		if f != (Fence{Owner: reclaimers[i][0], Generation: 2, Version: 2}) {
			t.Fatalf("round %d expired: fence %+v, reclaimer %s", i, f, reclaimers[i][0])
		}
	}
}

// N1 counterexample: another replica's startup recovery must not interrupt a build whose owner
// still holds a live lease. Only the expired build is reclaimed and interrupted, and the old
// owner's fence on it is refused afterwards while it keeps working on the live build.
func TestPostgres_ReplicaStartupKeepsLiveBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns helper processes")
	}
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	a := openPG(t, dsn)
	_, live := mustCreate(t, a, "live", "replica-a")
	liveRec, live, err := a.Transition(ctx, "live", live, StatusBuilding, "")
	if err != nil {
		t.Fatalf("Transition live: %v", err)
	}
	_, dead := mustCreate(t, a, "dead", "replica-a")
	_, dead, err = a.Transition(ctx, "dead", dead, StatusBuilding, "")
	if err != nil {
		t.Fatalf("Transition dead: %v", err)
	}
	expire(t, a, "dead", dead)

	results := runHelpers(t, dsn, "recover", "replica-b")
	if len(results) != 1 || results[0].result != "count=1 err=<nil>" {
		t.Fatalf("replica-b recovery: %+v, want count=1", results)
	}

	if got, gotFence := mustGet(t, a, "live"); got != liveRec || gotFence != live {
		t.Fatalf("live build was touched: %+v/%+v -> %+v/%+v", liveRec, live, got, gotFence)
	}
	deadRec, deadFence := mustGet(t, a, "dead")
	if deadRec.Status != StatusInterrupted || deadRec.FailureReason != recoverExpiredReason ||
		deadFence != (Fence{Owner: "replica-b", Generation: 2, Version: dead.Version + 1}) {
		t.Fatalf("dead build: %+v %+v", deadRec, deadFence)
	}
	if _, _, err := a.Transition(ctx, "dead", dead, StatusSucceeded, ""); !errors.Is(err, ErrAlreadyTerminal) {
		t.Fatalf("old owner terminal on recovered build: want ErrAlreadyTerminal, got %v", err)
	}
	if _, _, err := a.SetArtifact(ctx, "dead", dead, "harbor/x:1", "sha256:img"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old owner SetArtifact on recovered build: want ErrStaleFence, got %v", err)
	}
	if got, gotFence := mustGet(t, a, "dead"); got != deadRec || gotFence != deadFence {
		t.Fatalf("refused writes changed the recovered build: %+v %+v", got, gotFence)
	}
	if _, _, err := a.Transition(ctx, "live", live, StatusSucceeded, ""); err != nil {
		t.Fatalf("owner must keep working on its live build: %v", err)
	}
}
