---
name: run-firestore-tests
description: Run the emulator-gated Firestore test suites (session + park store). Use after changing session/park-store code or workflow event fields — these suites are excluded from the default `make ci` gate.
---

# Run Firestore Tests

The Firestore session/park-store backends are validated against the Cloud Firestore **emulator**, not the default unit run — `make cover` deliberately excludes them so nobody needs the emulator for everyday work. This skill runs that gated slice explicitly.

**Parameters**: none

**Usage examples**:
```text
/run-firestore-tests      # start emulator, run the suite, stop emulator
```

**The authoritative flow, backend matrix, and coverage rationale live in `okf/standards/testing.md` (Firestore-backed tests section) and `okf/tooling/ci-gates.md`.** This skill is a thin driver.

## When to Use

After any change touching:

- Session-store or park-store code (`*_firestore` backends, store conformance suites)
- Workflow event fields that get persisted (session history shape, park record fields)
- The `SESSION_BACKEND` switching logic or store construction in the setup layer

Not needed for changes that never touch persisted state — the default `make ci` covers those.

## Steps

1. **Start the emulator** (needs a JRE), capturing the PID so teardown kills exactly what
   was started. The standalone jar needs no gcloud SDK and is what CI uses:
   ```bash
   JAR=/tmp/firestore-emulator.jar
   SHA=9d43599ed6151199e8d604dc87fac51218e49e5f3a48519b1ae560bbe5e3382d  # v1.19.8
   test -f $JAR || curl -fsSL -o $JAR \
     https://storage.googleapis.com/firebase-preview-drop/emulator/cloud-firestore-emulator-v1.19.8.jar
   echo "$SHA  $JAR" | sha256sum -c - || { echo "emulator jar failed verification"; rm -f $JAR; exit 1; }

   java -jar $JAR --host=127.0.0.1 --port=8791 &
   EMULATOR_PID=$!

   # Poll rather than eyeball the log: the emulator takes a few seconds to bind, and step 2
   # fails confusingly if it starts first. Bounded, and it kills what it started on timeout.
   for i in $(seq 1 30); do
     curl -sf http://127.0.0.1:8791/ >/dev/null && break
     if [ "$i" = 30 ]; then kill $EMULATOR_PID; echo "emulator did not come up"; exit 1; fi
     sleep 1
   done
   ```
   The digest is checked because the next line runs the jar as code: the URL pins a version,
   but a version can be republished, and an unverified artifact would execute either way. Bump
   `SHA` alongside the URL when upgrading (`sha256sum` the downloaded file).

   (`gcloud emulators firestore start --host-port=127.0.0.1:8791` works too if you already have
   the SDK — then kill the process group at teardown, since the wrapper spawns a Java child.)

2. **Run the suite** (from `go/`):
   ```bash
   FIRESTORE_EMULATOR_HOST=127.0.0.1:8791 GOOGLE_CLOUD_PROJECT=test make cover-firestore
   ```

3. **Stop the emulator** — kill the process you started:
   ```bash
   kill "$EMULATOR_PID" 2>/dev/null || true
   ```
   Verify nothing is left listening: `lsof -i :8791` should be empty. If the PID was
   lost (different shell), fall back to `pkill -f 'firestore.*8791'` — scoped to this
   port so unrelated emulators survive.

4. Report pass/fail. On failure, fix and re-run step 2 (leave the emulator up between retries; still stop it when done).

## Key Rules

- **`make cover-firestore` fails fast with a hint if `FIRESTORE_EMULATOR_HOST` is unset** — that's expected, not a bug; set the var on the same command line.
- **These backends are skipped, not failed, without an emulator** — which is why running this explicitly matters rather than assuming green means covered: `internal/agent/setup` measures ~41% without it and ~82% with it, and the missing half is the cloud durability path production runs on.
- **Always stop the emulator when done** — a stale emulator on the port makes later runs hit old state.
- Test isolation is per-run (timestamped collection prefixes), so re-runs against a warm emulator are safe.
- These suites complement, not replace, the normal gates — run `/verify` as usual too.
