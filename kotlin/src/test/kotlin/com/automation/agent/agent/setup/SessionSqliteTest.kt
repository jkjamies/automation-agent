package com.automation.agent.agent.setup

import com.google.adk.kt.events.Event
import com.google.adk.kt.events.EventActions
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
import java.io.File

private fun tempDbPath(): String = File.createTempFile("session", ".db").apply { delete() }.absolutePath

private fun key(id: String) = SessionKey("app", "user", id)

private fun callEvent(callId: String): Event =
    Event(
        author = "model",
        content = Content(role = Role.MODEL, parts = listOf(Part(functionCall = FunctionCall(id = callId, name = "await_ci", args = mapOf("pr_number" to 7))))),
        longRunningToolIds = setOf(callId),
    )

class SessionSqliteTest : BehaviorSpec({
    Given("a sqlite session service") {
        When("creating, appending an event, then reopening on the same file") {
            Then("the session and its event survive (durability + Event round-trip)") {
                val path = tempDbPath()
                val s1 = SqliteSessionService(path)
                val session = s1.createSession(key("sess"), emptyMap())
                s1.appendEvent(session, callEvent("await_1"))

                // Reopen: a fresh service on the same file (the 'restart').
                val s2 = SqliteSessionService(path)
                val loaded = s2.getSession(key("sess"), GetSessionConfig()).shouldNotBeNull()
                loaded.events shouldHaveSize 1
                val call = loaded.events[0].content?.parts?.firstNotNullOfOrNull { it.functionCall }.shouldNotBeNull()
                call.id shouldBe "await_1"
                call.name shouldBe "await_ci"
                loaded.events[0].longRunningToolIds shouldBe setOf("await_1")
            }
        }

        When("createSession is given a blank id") {
            Then("a fresh id is generated") {
                val s = SqliteSessionService(tempDbPath())
                val session = s.createSession(key(""), emptyMap())
                session.key.id.shouldNotBeNull().isNotEmpty() shouldBe true
            }
        }

        When("an event carries a durable state delta") {
            Then("the state is persisted and reloaded; temp: keys are dropped") {
                val path = tempDbPath()
                val s1 = SqliteSessionService(path)
                val session = s1.createSession(key("sess"), emptyMap())
                val ev = Event(author = "model", content = Content(role = Role.MODEL, parts = listOf(Part(text = "hi"))), actions = EventActions(stateDelta = mutableMapOf("kept" to "v", "temp:gone" to "x")))
                s1.appendEvent(session, ev)

                val loaded = SqliteSessionService(path).getSession(key("sess"), GetSessionConfig()).shouldNotBeNull()
                loaded.state["kept"] shouldBe "v"
                loaded.state.containsKey("temp:gone") shouldBe false
            }
        }

        When("an event's state delta holds nested structures with nulls") {
            Then("the value shape (including nested nulls) round-trips on reload") {
                val path = tempDbPath()
                val s1 = SqliteSessionService(path)
                val session = s1.createSession(key("sess"), emptyMap())
                val delta = mutableMapOf<String, Any>("obj" to mapOf("a" to "x", "b" to null), "list" to listOf("1", null, "2"))
                s1.appendEvent(session, Event(author = "model", content = Content(role = Role.MODEL, parts = listOf(Part(text = "x"))), actions = EventActions(stateDelta = delta)))

                val loaded = SqliteSessionService(path).getSession(key("sess"), GetSessionConfig()).shouldNotBeNull()
                @Suppress("UNCHECKED_CAST")
                val obj = loaded.state["obj"] as Map<String, Any?>
                obj["a"] shouldBe "x"
                obj.containsKey("b") shouldBe true
                obj["b"] shouldBe null
                @Suppress("UNCHECKED_CAST")
                val list = loaded.state["list"] as List<Any?>
                list shouldBe listOf("1", null, "2")
            }
        }

        When("getSession is asked for an unknown session") {
            Then("it returns null") {
                SqliteSessionService(tempDbPath()).getSession(key("nope"), GetSessionConfig()) shouldBe null
            }
        }

        When("listing and deleting sessions") {
            Then("list reflects created sessions and delete removes them") {
                val s = SqliteSessionService(tempDbPath())
                s.createSession(key("a"), emptyMap())
                s.createSession(key("b"), emptyMap())
                s.listSessions("app", "user").sessions shouldHaveSize 2

                s.deleteSession(key("a"))
                s.listSessions("app", "user").sessions shouldHaveSize 1
                s.getSession(key("a"), GetSessionConfig()) shouldBe null
            }
        }

        When("numRecentEvents limits the returned history") {
            Then("only the most recent events are returned") {
                val s = SqliteSessionService(tempDbPath())
                val session = s.createSession(key("sess"), emptyMap())
                s.appendEvent(session, callEvent("c1"))
                s.appendEvent(session, callEvent("c2"))
                s.appendEvent(session, callEvent("c3"))

                val recent = s.getSession(key("sess"), GetSessionConfig(numRecentEvents = 2)).shouldNotBeNull()
                recent.events shouldHaveSize 2
                s.listEvents(key("sess")).events shouldHaveSize 3
            }
        }
    }
})
