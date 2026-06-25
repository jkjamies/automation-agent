package com.automation.agent.agent.setup

import com.google.adk.kt.events.Event
import com.google.adk.kt.sessions.GetSessionConfig
import com.google.adk.kt.sessions.SessionKey
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe

// Emulator-gated (FIRESTORE_EMULATOR_HOST); excluded from the coverage floor. Mirrors
// SessionSqliteTest against the cloud backend, including the Event round-trip + durability.
private val emulatorOn = System.getenv("FIRESTORE_EMULATOR_HOST") != null

private fun svc(collection: String) = FirestoreSessionService("demo-test", collection)

private fun newCollection() = "sessions_${System.nanoTime()}"

private fun fsKey(id: String) = SessionKey("app", "user", id)

private fun fsCallEvent(callId: String): Event =
    Event(
        author = "model",
        content = Content(role = Role.MODEL, parts = listOf(Part(functionCall = FunctionCall(id = callId, name = "await_ci", args = mapOf("pr_number" to 7))))),
        longRunningToolIds = setOf(callId),
    )

class FirestoreSessionServiceTest : BehaviorSpec({
    Given("a firestore session service") {
        When("creating, appending an event, then reading with a fresh instance") {
            Then("the session and its event survive (durability + Event round-trip)").config(enabled = emulatorOn) {
                val coll = newCollection()
                val s1 = svc(coll)
                val session = s1.createSession(fsKey("sess"), emptyMap())
                s1.appendEvent(session, fsCallEvent("await_1"))

                val loaded = svc(coll).getSession(fsKey("sess"), GetSessionConfig()).shouldNotBeNull()
                loaded.events shouldHaveSize 1
                val call = loaded.events[0].content?.parts?.firstNotNullOfOrNull { it.functionCall }.shouldNotBeNull()
                call.id shouldBe "await_1"
                call.name shouldBe "await_ci"
                loaded.events[0].longRunningToolIds shouldBe setOf("await_1")
            }
        }

        When("getSession is asked for an unknown session") {
            Then("it returns null").config(enabled = emulatorOn) {
                svc(newCollection()).getSession(fsKey("nope"), GetSessionConfig()) shouldBe null
            }
        }

        When("listing and deleting sessions") {
            Then("list reflects created sessions and delete removes them").config(enabled = emulatorOn) {
                val s = svc(newCollection())
                s.createSession(fsKey("a"), emptyMap())
                s.createSession(fsKey("b"), emptyMap())
                s.listSessions("app", "user").sessions shouldHaveSize 2
                s.deleteSession(fsKey("a"))
                s.listSessions("app", "user").sessions shouldHaveSize 1
                s.getSession(fsKey("a"), GetSessionConfig()) shouldBe null
            }
        }

        When("numRecentEvents limits the returned history") {
            Then("only the most recent events are returned").config(enabled = emulatorOn) {
                val s = svc(newCollection())
                val session = s.createSession(fsKey("sess"), emptyMap())
                s.appendEvent(session, fsCallEvent("c1"))
                s.appendEvent(session, fsCallEvent("c2"))
                s.appendEvent(session, fsCallEvent("c3"))
                s.getSession(fsKey("sess"), GetSessionConfig(numRecentEvents = 2)).shouldNotBeNull().events shouldHaveSize 2
                s.listEvents(fsKey("sess")).events shouldHaveSize 3
            }
        }
    }
})
