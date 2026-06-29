package com.automation.agent.tasks

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout
import java.time.Instant
import java.util.concurrent.atomic.AtomicBoolean

private fun env(source: String = "webhook:/lint") =
    Envelope.new(Kind.LINT, source, "hi".toByteArray(), Instant.EPOCH)

class InProcessTest : BehaviorSpec({
    Given("an in-process transport") {
        When("enqueueing an envelope") {
            Then("it is handed to the dispatcher") {
                withTimeout(5_000) {
                    val seen = CompletableDeferred<Envelope>()
                    val t = InProcess({ e -> seen.complete(e) })
                    t.enqueue(env())
                    seen.await() shouldBe env()
                    t.close()
                }
            }
        }
    }

    Given("a closed transport") {
        When("enqueueing after close") {
            Then("it is rejected") {
                val t = InProcess({ })
                t.close()
                shouldThrow<TransportClosedException> { t.enqueue(env()) }
            }
        }
    }

    Given("a transport with an in-flight dispatch") {
        When("close races a running dispatch") {
            Then("close waits for the dispatch to finish (drain)") {
                withTimeout(5_000) {
                    val started = CompletableDeferred<Unit>()
                    val release = CompletableDeferred<Unit>()
                    val finished = AtomicBoolean(false)
                    val t = InProcess({
                        started.complete(Unit)
                        release.await()
                        finished.set(true)
                    })
                    t.enqueue(env())
                    started.await()
                    coroutineScope {
                        val closing = launch { t.close() }
                        release.complete(Unit)
                        closing.join()
                    }
                    finished.get() shouldBe true
                }
            }
        }
    }

    Given("a single-slot transport whose only permit is held") {
        When("an enqueue parks on the permit while close begins (recheck-after-acquire)") {
            Then("the parked enqueue is rejected once shutdown starts, never dispatched") {
                withTimeout(5_000) {
                    val firstRunning = CompletableDeferred<Unit>()
                    val gate = CompletableDeferred<Unit>()
                    val secondRan = AtomicBoolean(false)
                    val secondRejected = AtomicBoolean(false)
                    val t = InProcess(
                        { e ->
                            if (e.source == "first") {
                                firstRunning.complete(Unit)
                                gate.await()
                            } else {
                                secondRan.set(true)
                            }
                        },
                        maxConcurrent = 1,
                    )
                    // The first dispatch takes the only permit (acquired synchronously in enqueue)
                    // and parks on the gate; await firstRunning so the permit is provably held before
                    // the second enqueue contends for it.
                    t.enqueue(env("first"))
                    firstRunning.await()
                    coroutineScope {
                        val second = launch {
                            try {
                                t.enqueue(env("second"))
                            } catch (_: TransportClosedException) {
                                secondRejected.set(true)
                            }
                        }
                        // close() flips the closed flag (under the mutex) before it drains, and the
                        // gate keeps the permit held until after close has begun — so the permit can
                        // only free *after* shutdown started. The second enqueue therefore always
                        // observes closed (whether it parked on the permit first and rechecks, or
                        // sees it at the top guard) and is rejected without ever dispatching, for
                        // either interleaving. The short wait only biases toward the recheck path.
                        val closing = launch { t.close() }
                        delay(100)
                        gate.complete(Unit)
                        second.join()
                        closing.join()
                    }
                    secondRejected.get() shouldBe true
                    secondRan.get() shouldBe false
                }
            }
        }
    }

    Given("a single-slot transport whose only permit never frees") {
        When("an enqueue is parked on the permit and close begins") {
            Then("the parked enqueue wakes and is rejected without waiting for a release") {
                withTimeout(5_000) {
                    val firstRunning = CompletableDeferred<Unit>()
                    val neverFrees = CompletableDeferred<Unit>() // the in-flight dispatch never completes
                    val secondRejected = AtomicBoolean(false)
                    val t = InProcess(
                        { e ->
                            if (e.source == "first") {
                                firstRunning.complete(Unit)
                                neverFrees.await() // holds the only permit for the whole test
                            }
                        },
                        maxConcurrent = 1,
                        drainTimeoutMs = 50,
                    )
                    // The first dispatch takes the only permit and never releases it, so the second
                    // enqueue can only unblock by being woken when close() begins — never by a freed
                    // permit. This is the guarantee the slot-vs-close race adds.
                    t.enqueue(env("first"))
                    firstRunning.await()
                    coroutineScope {
                        val second = launch {
                            try {
                                t.enqueue(env("second"))
                            } catch (_: TransportClosedException) {
                                secondRejected.set(true)
                            }
                        }
                        t.close() // completes the close signal, waking the parked second enqueue
                        second.join()
                    }
                    secondRejected.get() shouldBe true
                    // Let the still-parked first dispatch finish so no coroutine outlives the scope.
                    neverFrees.complete(Unit)
                }
            }
        }
    }

    Given("a transport whose dispatch never finishes") {
        When("close exceeds the drain budget") {
            Then("it stops waiting rather than hanging, leaving the dispatch in flight") {
                withTimeout(5_000) {
                    val started = CompletableDeferred<Unit>()
                    val release = CompletableDeferred<Unit>()
                    val finished = CompletableDeferred<Unit>()
                    val t = InProcess(
                        {
                            started.complete(Unit)
                            try {
                                release.await()
                            } finally {
                                finished.complete(Unit)
                            }
                        },
                        drainTimeoutMs = 50,
                    )
                    t.enqueue(env())
                    started.await()
                    t.close() // returns after the 50ms drain budget instead of blocking forever
                    // Release the still-running dispatch so no coroutine outlives the test scope.
                    release.complete(Unit)
                    finished.await()
                }
            }
        }
    }

    Given("a max-concurrent below one") {
        When("constructing the transport") {
            Then("it still accepts and dispatches (the floor is applied)") {
                withTimeout(5_000) {
                    val seen = CompletableDeferred<Unit>()
                    val t = InProcess({ seen.complete(Unit) }, maxConcurrent = 0)
                    t.enqueue(env())
                    seen.await()
                    t.close()
                }
            }
        }
    }
})
