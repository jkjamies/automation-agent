package com.automation.agent.agent.setup

import com.google.adk.kt.types.Role
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe

class EventsTest : BehaviorSpec({
    Given("a user-text content") {
        When("reading its role and text") {
            Then("the role is user and ContentText returns the text") {
                val c = userText("hello")
                c.role shouldBe Role.USER
                contentText(c) shouldBe "hello"
            }
        }
        When("reading a null content") {
            Then("ContentText is empty") {
                contentText(null) shouldBe ""
            }
        }
    }

    Given("a list of contents") {
        When("taking the last text") {
            Then("an empty list yields empty and a populated list yields the final text") {
                lastText(emptyList()) shouldBe ""
                lastText(listOf(userText("first"), userText("last"))) shouldBe "last"
            }
        }
    }

    Given("the TextEvent helper") {
        When("building events with and without state") {
            Then("it carries author, text, and an optional state delta") {
                val ev = textEvent("author", "body", mapOf("key" to "val"))
                ev.author shouldBe "author"
                contentText(ev.content) shouldBe "body"
                ev.actions.stateDelta["key"] shouldBe "val"

                val plain = textEvent("a", "b", null)
                plain.actions.stateDelta.isEmpty() shouldBe true
            }
        }
    }

    Given("a state map") {
        When("reading string values") {
            Then("strings return, non-strings and missing keys yield empty") {
                val s = mapOf<String, Any?>("a" to "x", "b" to 42)
                stateString(s, "a") shouldBe "x"
                stateString(s, "b") shouldBe ""
                stateString(s, "missing") shouldBe ""
            }
        }
    }
})
