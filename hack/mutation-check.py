#!/usr/bin/env python3
"""Mutation check: break one guard at a time, assert its test catches it.

    python3 hack/mutation-check.py            # all mutations
    python3 hack/mutation-check.py session    # one group
    python3 hack/mutation-check.py -l         # list without running

Why this exists
---------------
A test that passes the first time it is written has demonstrated nothing
— it has only shown that the code and the test agree today. Every guard
in this repo exists because something went wrong in production once, so
each one gets an entry below that breaks it deliberately and asserts the
corresponding test goes red with a message a human can act on.

Three failure modes this harness reports, which are easy to confuse:

  NOT CAUGHT    the test passes with the guard broken. The test is
                decorative. Fix the test.
  INVALID       the mutated code does not compile (typically an
                unused variable), so nothing was proven either way.
                Fix the mutation, not the test.
  STALE         the `find` string does not occur the expected number of
                times. The code moved; update the entry.
  HUNG          the test deadlocks instead of failing. Worse than no
                test: it burns the whole CI timeout and reports
                nothing. Fix the test's synchronisation.

It covers BOTH the service and plugin modules. Ported from
sc-component-notifier's hack/mutation-check.py — adopting the harness
shape verbatim per sc-components roadmap item
`component-repos-have-no-mutation-harness`.

The TWO version-wire entries at the bottom of MUTATIONS are the ones
this repo earns its own harness for: a value present, correct, and
read by nothing, logging identically to a working deploy. That exact
shape has shipped twice in this fleet (`signingKey` as a bare ARN on
backup-smoke-1d 2026-11-14, then BACKUP_OPERATOR_PASSWORD on the first
backup.simple-forge.com login 2026-11-18). The binary test
TestBinaryReportsItsBuildOnHealthzAndSession is the only honest
assertion of the end-to-end wire; these entries assert it stays honest.

The working tree is restored after every mutation, including on failure
and on Ctrl-C. Run it on a clean tree anyway: a crash mid-write would
leave one file mutated, and `git diff` is the check.
"""

import pathlib
import shutil
import signal
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The repo is three Go modules and the guards worth mutating live in two
# of them. File paths below are REPO-relative and their first segment
# names the module, which is also the directory `go test` runs in — a
# plugin mutation tested from service/ would simply not find the package.
# A cross-module contract test is the one exception and says so by
# prefixing its package with the module it runs in ("plugin:./cmd/...").

API_PKG = "./internal/api/"
ADMIN_PKG = "./internal/goembedbadmin/"
STORAGE_PKG = "./internal/backup/storage/"
PLUGIN_PKG = "./internal/"
PLG_CMD_PKG = "./cmd/sc-component-backup/"
# The SERVICE module's cmd package. Easy to confuse with PLG_CMD_PKG
# above — both are "the cmd package", and reaching for the wrong one
# does not fail loudly: the mutation still applies (the find string is
# in the service's main.go), the harness runs a test package in the
# OTHER module where that test name does not exist, `go test -run`
# reports "no tests to run" and exits 0, and the entry lands as NOT
# CAUGHT rather than as a mistake in the entry.
SVC_CMD_PKG = "./cmd/backup/"

# A test package that lives in a DIFFERENT module from the file being
# mutated is written "<module>:<pkg>" — see the module resolution in
# main(). Only one guard needs it, and it needs it for a real reason:
# the secret-env binding is declared in the plugin module and consumed
# in the service module, so the contract test that spans them can only
# live on one side.
PLG_CMD_PKG_XMOD = "plugin:" + PLG_CMD_PKG

# service/ files
HEALTHZ = "service/internal/api/healthz.go"
# Backup names its session file session_handlers.go (not session.go).
SESSION = "service/internal/api/session_handlers.go"
JWTMW = "service/internal/api/jwt_middleware.go"
TICK = "service/internal/api/scheduler_tick.go"
TENANTS_REGISTER = "service/internal/api/tenants_register.go"
ADMIN = "service/internal/goembedbadmin/migration.go"
# The sibling colocated-source migration — same package, different file.
# Kept alongside ADMIN so a reviewer sees both guard surfaces in one
# place. The two live-caught traps the file exists to close
# (verified=true on empty, source_empty response field) are mutated
# below.
COLOC_ADMIN = "service/internal/goembedbadmin/colocated_source.go"
# Colocated meta-store backend reporter — the live wire the Prometheus
# `backup_store_backend` gauge now reads at boot (set-once-from-env was
# replaced with set-once-from-metaStore.Backend()). A regression here
# would silently mislabel the gauge after a `metadata: colocated`
# deploy and read the wrong answer under an unauthenticated
# `curl /metrics | grep store_backend` — the same shape of probe-read-
# wrong-field that misled the pre-fix framing of this feature.
COLOC_META = "service/internal/backup/meta/colocated.go"
META_PKG = "./internal/backup/meta/"
S3_STORAGE = "service/internal/backup/storage/s3.go"
SVC_MAIN = "service/cmd/backup/main.go"

# plugin/ files
PLG_MAIN = "plugin/cmd/sc-component-backup/main.go"

# (group, name, file, test-package, find, replace, test-regex, occurrences)
MUTATIONS = [
    # --------------------------------------------------------------- session
    # The operator login flow and the op-gating it makes possible.
    # These mirror notifier's session-handler entries where backup's
    # test surface covers the same guard; where backup has no handler
    # test for a guard, the binary test in service/cmd/backup picks it
    # up live (see the version-wire entries at the bottom).

    # Live-caught 2026-11-24 writing this entry: the obvious target
    # (TestTenantsRegister_JWTMismatch) catches the mutation but ONLY
    # because its token happens to be a register-op token for tenant
    # "a" with body stack "b" — so the tenant↔body check below fires
    # first. Every other existing test uses mintTestJWT which hard-
    # codes op:"register". Added TestTenantsRegister_RejectsNonRegisterOp
    # with an operator-op, tenant-matching token precisely so this
    # mutation targets the op check and nothing else.
    ("session", "tenants_register op check removed", TENANTS_REGISTER, API_PKG,
     'if claims == nil || claims.Op != "register" {',
     "if claims == nil {",
     "TestTenantsRegister_RejectsNonRegisterOp", 1),

    # The JWT claim ↔ body cross-check: a consumer's own register token
    # must not be usable to register against another consumer's name.
    # Live-caught in notifier's equivalent path; same shape here.
    ("session", "register claim ↔ body cross-check removed", TENANTS_REGISTER, API_PKG,
     "if claims.Tenant != record.StackName {",
     "if false {",
     "TestTenantsRegister_JWTMismatch", 1),

    # ------------------------------------------------------------------ tick
    # The scheduler route spawns an ECS Fargate task per due tenant —
    # every consumer's register token would be enough to drain the
    # account on demand without this op check. Mirror of notifier's
    # delivery-tick entry.
    ("tick", "any token may drive the scheduler tick", TICK, API_PKG,
     "if claims := ClaimsFromContext(ctx); claims == nil ||\n\t\t\t(claims.Op != OpScheduler && claims.Op != OpOperator) {",
     "if claims := ClaimsFromContext(ctx); claims == nil {",
     "TestSchedulerTick_RejectsNonSchedulerOps", 1),

    # ------------------------------------------------------- goembedbadmin
    # The migration endpoint reads every key out of the source Mongo
    # and upserts it into the live store, and it is deliberately NOT
    # behind the service JWT. The three checks at the top of Migrate
    # are its entire access control. Mirror of notifier's three
    # goembedbadmin entries.
    ("goembedbadmin", "migration token compare accepts anything", ADMIN, ADMIN_PKG,
     "[]byte(c.AdminToken)) != 1 {",
     "[]byte(c.AdminToken)) == -1 {",
     "TestMigration_AuthAndGate", 1),

    # The other direction: a compare that rejects a correct token would
    # make the cutover impossible, and "my token is wrong" is the first
    # thing an operator would believe.
    ("goembedbadmin", "migration token compare rejects everything", ADMIN, ADMIN_PKG,
     "[]byte(c.AdminToken)) != 1 {",
     "[]byte(c.AdminToken)) != -1 {",
     "TestMigration_AuthAndGate", 1),

    # Removing the opt-in gate does not open the route to anyone with a
    # token — it opens it to a request with NO header at all, because an
    # empty configured token and an empty header compare equal. So the
    # gate is not politeness about 404-vs-401; it is the thing standing
    # between an unconfigured deploy and an unauthenticated migration.
    ("goembedbadmin", "opt-in gate removed", ADMIN, ADMIN_PKG,
     '\tif c.AdminToken == "" {\n'
     '\t\twriteErr(w, http.StatusNotFound, "migration endpoint disabled (ADMIN_MIGRATION_TOKEN unset)")\n'
     "\t\treturn\n\t}\n",
     "",
     "TestMigration_AuthAndGate", 1),

    # The colocated → go-embedb migration exists because an operator
    # flipping USE_GOEMBEDB=1 on a `metadata: colocated` staging deploy
    # would have otherwise been served by the mongo-source endpoint
    # above — which counts an EMPTY external DB, declares `verified=true`
    # against `0 >= 0`, and leaves every tenant orphaned on the next
    # scheduler tick. The two guards below are the specific surface that
    # closes that trap: `SourceCount > 0` is the AND-term keeping verified
    # honest, and the 422 status on an empty wet run is what makes a
    # runbook `curl -f` catch the orphan path without parsing JSON.
    ("goembedbadmin", "colocated verified drops SourceCount>0 term", COLOC_ADMIN, ADMIN_PKG,
     "if !dryRun && len(resp.Errors) == 0 && resp.SourceCount > 0 {",
     "if !dryRun && len(resp.Errors) == 0 {",
     "TestColocatedMigration_EmptySource_422", 1),

    # The 422-on-empty guard itself. A regression to 200 still returns
    # source_empty=true in the body, but shell `curl -f` would no longer
    # catch it — an operator automation that assumes non-2xx = failure
    # would flip USE_GOEMBEDB=1 on an empty store.
    ("goembedbadmin", "colocated empty-source stays 200 (loses 422)", COLOC_ADMIN, ADMIN_PKG,
     "if resp.SourceEmpty && !dryRun {\n\t\tstatus = http.StatusUnprocessableEntity\n\t}",
     "",
     "TestColocatedMigration_EmptySource_422", 1),

    # The `backup_store_backend` Prometheus gauge now reads the live
    # metaStore.Backend() post-boot (instead of the old set-once-from-env
    # inference). If colocatedStore.Backend() ever returns the empty
    # TypeDefault — what resolveType bounces the empty env back to as
    # "colocated" — the gauge would report "" after a `metadata: colocated`
    # deploy and an unauthenticated `curl /metrics | grep store_backend`
    # would read the wrong answer. This is the specific probe the gauge
    # exists to answer honestly after a `USE_GOEMBEDB=1` cutover.
    ("goembedbadmin", "colocated.Backend() mis-reports as TypeDefault", COLOC_META, META_PKG,
     "func (s *colocatedStore) Backend() Type                      { return TypeColocated }",
     "func (s *colocatedStore) Backend() Type                      { return TypeDefault }",
     "TestColocated_BackendReportsLive", 1),

    # ------------------------------------------------------- secret-env wire
    # Schema() is the component's published API and it is two plain maps
    # with nothing keeping them consistent. Both ways they drift are
    # silent at deploy time.

    # Core dual-uses ComponentAllowedOverrides as the allowed-top-level-
    # keys check, so a bound key that is not on it is refused at PARSE
    # time with "unknown or disallowed top-level key" — the operator
    # cannot write the line, and the binding can never fire.
    ("plugin", "secret bound to a key the operator cannot write", PLG_MAIN, PLG_CMD_PKG,
     '\t\t\t\t"migrationToken",\n',
     "",
     "TestComponentSecretEnvMapKeysAreWritable", 1),

    # Two entries bound to one env var: the second silently wins the
    # map write, and one of the config fields compares against the
    # wrong secret forever. Mirrors notifier's shape.
    ("plugin", "two config keys bound to one env var", PLG_MAIN, PLG_CMD_PKG,
     '"migrationToken": "ADMIN_MIGRATION_TOKEN"',
     '"migrationToken": "SERVICE_JWT_SECRET"',
     "TestSecretEnvVarNamesAreSane", 1),

    # The cross-module one, and the only mutation here that names its
    # test's module explicitly. A bound secret arrives as a
    # SecretsManager ARN; a service that reads it with a bare os.Getenv
    # compares against the ARN string forever, logs nothing, and looks
    # exactly like a working deploy. Live-caught twice already in this
    # fleet — signingKey on backup-smoke-1d 2026-11-14, then
    # BACKUP_OPERATOR_PASSWORD on the first backup.simple-forge.com
    # login 2026-11-18.
    #
    # The service-side resolver is a block guarded by `os.Getenv(...) != ""`,
    # so `resolveCtx` and `cancel` are declared before the line we
    # mutate. Preserving both with `_ =` keeps the mutated code compiling
    # (otherwise the harness reports INVALID rather than NOT CAUGHT).
    ("plugin", "bound secret read as raw env by the service", SVC_MAIN, PLG_CMD_PKG_XMOD,
     '\t\tresolved, err := resolveAWSSecretARN(resolveCtx, "ADMIN_MIGRATION_TOKEN", t)',
     "\t\t_ = resolveCtx\n\t\tresolved, err := t, error(nil)",
     "TestSecretEnvVarsAreResolvedByTheService", 1),

    # ------------------------------------------------- build-stamp wires
    # The build stamp. Three guards, and the surface that matters is
    # /healthz: it is the only probe a post-deploy smoke can reach with
    # no credential at all, which is the whole reason the field exists.
    #
    # Backup has NO fine-grained healthz_test.go / session_handlers_test.go
    # (every internal/api test builds its own SessionConfig, so none
    # catches main.go never passing version at all). The binary test
    # TestBinaryReportsItsBuildOnHealthzAndSession is the only honest
    # assertion of both wires; all three healthz/session guards below
    # target that same binary test.

    ("session", "healthz stops reporting the build", HEALTHZ, SVC_CMD_PKG,
     '\t\t\t"version": version,\n',
     "",
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    # The status field is what the Lambda health check and HTTP smokes
    # match on, and renaming it would leave a 200 with a plausible-
    # looking body — a probe that passes while saying nothing.
    ("session", "healthz stops saying ok", HEALTHZ, SVC_CMD_PKG,
     '"status":  "ok",',
     '"state":  "ok",',
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    ("session", "session probe stops reporting the build", SESSION, SVC_CMD_PKG,
     "\t\tVersion:     c.Version,\n",
     "",
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    # And the two wires. Both are in main.go and NEITHER is visible to
    # a handler test: every test in internal/api constructs its own
    # handler, so all of them pass with main.go never passing `version`
    # at all. That is the exact shape of the bug this fleet has shipped
    # twice (above). Hence a binary test.
    ("session", "healthz wired without the version", SVC_MAIN, SVC_CMD_PKG,
     'router.Get("/healthz", api.Healthz(version))',
     'router.Get("/healthz", api.Healthz(""))',
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    ("session", "session wired without the version", SVC_MAIN, SVC_CMD_PKG,
     "sessionCfg.Version = version",
     'sessionCfg.Version = ""',
     "TestBinaryReportsItsBuildOnHealthzAndSession", 1),

    # ARN-exit calibration (per sc-components' selection-bias correction
    # 2026-11-24): the two bugs backup actually shipped — `signingKey`
    # as a bare ARN on backup-smoke-1d 2026-11-14, and
    # BACKUP_OPERATOR_PASSWORD read-by-nothing on backup.simple-forge.com
    # 2026-11-18 — were closed by adding resolveAWSSecretARN + a loud
    # os.Exit(2) on resolve failure. Those exits are the guard this
    # whole file exists to assert. Each mutation deletes the Exit(2)
    # line inside its respective resolve-error block; the test boots
    # with a bogus ARN and asserts exit code 2, so a bypass of the
    # loud-failure path flips the test red.
    ("arn-exit", "SERVICE_JWT_SECRET resolve failure no longer exits", SVC_MAIN, SVC_CMD_PKG,
     'slog.Error("failed to resolve SERVICE_JWT_SECRET from SecretsManager", "err", err)\n\t\t\tos.Exit(2)',
     'slog.Error("failed to resolve SERVICE_JWT_SECRET from SecretsManager", "err", err)',
     "TestBinaryExitsTwoOnServiceJWTSecretArnResolveFailure", 1),

    ("arn-exit", "BACKUP_OPERATOR_PASSWORD resolve failure no longer exits", SVC_MAIN, SVC_CMD_PKG,
     'slog.Error("failed to resolve BACKUP_OPERATOR_PASSWORD from SecretsManager", "err", err)\n\t\t\tos.Exit(2)',
     'slog.Error("failed to resolve BACKUP_OPERATOR_PASSWORD from SecretsManager", "err", err)',
     "TestBinaryExitsTwoOnBackupOperatorPasswordArnResolveFailure", 1),

    # --------------------------------------------------------- s3-pagination
    # The listAllObjects pagination loop. The pre-2026-11-24 shape
    # issued one `ListObjectsV2` call and returned the 1000-key page as
    # the complete listing — live-caught on sibling
    # sc-component-observatory with `s3_count: 1000` exactly at the AWS
    # page cap. On any backup deployment past 1000 keys for a
    # `_meta/<domain>/` prefix the SPA list-runs query and the scheduler
    # "has this tenant been backed up recently?" probe both lost
    # visibility, and PR #49's colocated → go-embedb migration derived
    # both SourceCount AND MigratedCount from the truncated page so
    # `verified: true` stayed honest against itself, orphaning tenants
    # on cutover.
    #
    # Mutation: force the termination check to always exit after
    # page 1. The unique line below only matches the paginating shape
    # (the pre-fix one-call code never reads IsTruncated at all), so a
    # STALE here means the fix has been reverted and the entry wants
    # updating to the new find string — exactly the signal this harness
    # is built for.
    #
    # TestS3Store_List_PaginatesPastPageCap catches on three assertions
    # simultaneously (len 1000 vs 1001, one call vs two, and the missing
    # page-2 token) — a count-only assertion would pass against a
    # fake that ignored the token, which is why the test's fake REFUSES
    # unknown tokens and records the ContinuationToken per call.
    ("s3-pagination", "listAllObjects stops following ContinuationToken", S3_STORAGE, STORAGE_PKG,
     'if out.IsTruncated == nil || !*out.IsTruncated || aws.ToString(out.NextContinuationToken) == "" {',
     "if true {",
     "TestS3Store_List_PaginatesPastPageCap", 1),

    # Prefix-stability entry. Mutating `aws.String(prefix)` on every
    # page to `nil` makes page 2 list the whole bucket — a SUPERSET of
    # the caller's request. The count assertion stays green against a
    # fake with no extra keys outside the prefix; only the explicit
    # prefix-per-call recording catches it. See the file-header
    # ground-truth note in s3_pagination_test.go.
    ("s3-pagination", "paginated prefix sent on page 1 only", S3_STORAGE, STORAGE_PKG,
     "Prefix:            aws.String(prefix),",
     "Prefix:            nil,",
     "TestS3Store_List_PaginatedPrefixIsStable", 1),
]

TEST_FILE_PREFIXES = (
    "tenants_register_test.go:",
    "arn_exit_test.go:",
    "scheduler_tick_test.go:",
    "migration_test.go:",
    "main_test.go:",
    "version_wire_test.go:",
    "s3_pagination_test.go:",
)
COMPILE_MARKERS = ("build failed", "undefined:", "declared and not used", "cannot use")


def first_message(out: str) -> tuple[str, bool]:
    """Return (message, is_compile_error) from a go test failure.

    A t.Fatalf message sits on the same line as the file:line prefix, but
    gomega puts nothing there and wraps its "Expected / to equal" report
    across the following indented lines. Both shapes have to read as one
    sentence here, or the api-package mutations report a bare line number
    and the operator has to re-run the test by hand to learn what broke.
    """
    lines = out.splitlines()
    for i, line in enumerate(lines):
        s = line.strip()
        if s.startswith(TEST_FILE_PREFIXES):
            msg = s.split(": ", 1)[-1] if ": " in s else ""
            if msg:
                return msg, False
            tail = [t.strip() for t in lines[i + 1:i + 7]
                    if t.strip() and not t.startswith(("FAIL", "ok", "---", "===", "exit status"))]
            return " ".join(tail) or s, False
        if any(m in s for m in COMPILE_MARKERS):
            return s, True
    return "", False


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    listing = "-l" in sys.argv or "--list" in sys.argv

    muts = [m for m in MUTATIONS if not args or m[0] in args]
    if not muts:
        groups = sorted({m[0] for m in MUTATIONS})
        print(f"no mutations match {args}; groups are: {', '.join(groups)}")
        return 2

    if listing:
        for group, name, _, _, _, _, test, _ in muts:
            print(f"{group:16} {name:56} {test}")
        return 0

    # SIGTERM does not raise, so the restore in `finally` below never runs
    # and the tree is left with exactly one file mutated — which then shows
    # up as an unrelated failure in whatever runs next. Turning it into
    # SystemExit makes the restore happen.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(130))

    # Back up exactly the files this run will mutate, rather than each
    # module's whole internal/ tree. Guards worth mutating also live
    # OUTSIDE internal/ — Schema's allowlist and secret-env map in
    # plugin/cmd/, the secret resolve in service/cmd/ — and with a
    # tree copy keyed on internal/ the restore below raises
    # FileNotFoundError for those and leaves the file mutated. Backing
    # up by path covers both, and the harness only ever writes files
    # named in MUTATIONS, so it loses no insurance.
    backup = pathlib.Path(tempfile.mkdtemp(prefix="mutation-check-"))
    mutated_files = sorted({m[2] for m in muts})
    for rel in mutated_files:
        (backup / rel).parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / rel, backup / rel)

    problems = []
    try:
        for group, name, rel, pkg, find, repl, test, want in muts:
            label = f"{group}/{name}"
            path = ROOT / rel
            # The module the file belongs to is normally also the
            # directory the test has to run in: each module has its own
            # go.mod, so `go test ./internal/` resolves differently per
            # module. The exception is a cross-module contract test (the
            # plugin asserting something about the service's source),
            # which names its module as "<module>:<pkg>".
            if ":" in pkg:
                mod_name, pkg = pkg.split(":", 1)
            else:
                mod_name = rel.split("/", 1)[0]
            module = ROOT / mod_name
            src = path.read_text()
            if src.count(find) != want:
                print(f"STALE       {label}")
                print(f"              pattern occurs {src.count(find)}x, expected {want} — "
                      f"the code moved; update the mutation")
                problems.append(label)
                continue

            path.write_text(src.replace(find, repl))
            try:
                proc = subprocess.run(
                    ["go", "test", pkg, "-count=1", "-run", test],
                    cwd=module, capture_output=True, text=True, timeout=240,
                )
            except subprocess.TimeoutExpired:
                print(f"HUNG        {label}")
                print("              the test deadlocks instead of failing — it would burn the CI timeout")
                problems.append(label)
                continue
            finally:
                shutil.copyfile(backup / rel, path)

            if proc.returncode == 0:
                print(f"NOT CAUGHT  {label}")
                print(f"              {test} PASSES with the guard broken — it proves nothing")
                problems.append(label)
                continue

            msg, compile_err = first_message(proc.stdout + proc.stderr)
            if compile_err:
                print(f"INVALID     {label}")
                print(f"              the mutation does not compile, so nothing was proven: {msg}")
                problems.append(label)
                continue

            print(f"caught      {label}")
            if msg:
                print(f"              → {msg[:200]}")
    finally:
        # Belt and braces: restore every file this run could have
        # touched, so a crash or a Ctrl-C mid-run cannot leave a mutated
        # file behind.
        for rel in mutated_files:
            shutil.copyfile(backup / rel, ROOT / rel)
        shutil.rmtree(backup, ignore_errors=True)

    print()
    if problems:
        print(f"{len(problems)} of {len(muts)} not proven:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print(f"all {len(muts)} mutations caught")
    return 0


if __name__ == "__main__":
    sys.exit(main())
