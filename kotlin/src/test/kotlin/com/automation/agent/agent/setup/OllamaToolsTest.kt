package com.automation.agent.agent.setup

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FinishReason
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.FunctionResponse
import com.google.adk.kt.types.GenerateContentConfig
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Tool
import com.google.adk.kt.types.Type
import com.google.adk.kt.types.FunctionDeclaration
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put

class OllamaToolsTest : BehaviorSpec({
    Given("generation options") {
        When("derived from config") {
            Then("defaults are deterministic and config values are honored") {
                val defaults = generationOptions(GenerateContentConfig())
                defaults.getValue("num_ctx").jsonPrimitive.int shouldBe 32768
                defaults.getValue("temperature").jsonPrimitive.double shouldBe 0.0

                val honored = generationOptions(GenerateContentConfig(temperature = 0.5f, topP = 0.9f))
                honored.getValue("temperature").jsonPrimitive.double shouldBe 0.5
                (honored.getValue("top_p").jsonPrimitive.double in 0.89..0.91) shouldBe true
            }
        }
    }

    Given("the JSON-format switch") {
        When("inspecting the response MIME type") {
            Then("only an application/json type wants JSON") {
                wantsJSON(GenerateContentConfig()) shouldBe false
                wantsJSON(GenerateContentConfig(responseMimeType = "application/json")) shouldBe true
            }
        }
    }

    Given("a function declaration") {
        When("converting to Ollama tools") {
            Then("name, type, required and lowercased property types are projected") {
                val cfg =
                    GenerateContentConfig(
                        tools =
                            listOf(
                                Tool(
                                    functionDeclarations =
                                        listOf(
                                            FunctionDeclaration(
                                                name = "read_file",
                                                description = "read a repo file",
                                                parameters =
                                                    Schema(
                                                        type = Type.OBJECT,
                                                        properties = mapOf("path" to Schema(type = Type.STRING, description = "the path")),
                                                        required = listOf("path"),
                                                    ),
                                            ),
                                        ),
                                ),
                            ),
                    )
                val tools = toOllamaTools(cfg).shouldNotBeNull()
                tools.size shouldBe 1
                tools[0].jsonObject.getValue("type").jsonPrimitive.content shouldBe "function"
                val fn = tools[0].jsonObject.getValue("function").jsonObject
                fn.getValue("name").jsonPrimitive.content shouldBe "read_file"
                val params = fn.getValue("parameters").jsonObject
                params.getValue("type").jsonPrimitive.content shouldBe "object"
                params.getValue("required").jsonArray.size shouldBe 1
                val pathProp = params.getValue("properties").jsonObject.getValue("path").jsonObject
                pathProp.getValue("type").jsonArray[0].jsonPrimitive.content shouldBe "string"

                toOllamaTools(GenerateContentConfig()) shouldBe null
            }
        }
    }

    Given("a final response with a tool call") {
        When("building it") {
            Then("it is complete and carries text + function-call parts") {
                val tc = OllamaToolCall(OllamaToolCallFunction(name = "get_weather", arguments = buildJsonObject { put("city", "Paris") }))
                val resp = finalResponse("here you go", listOf(tc), "gemma4")
                resp.finishReason shouldBe FinishReason.STOP
                val parts = resp.content.shouldNotBeNull().parts
                parts.size shouldBe 2
                parts[0].text shouldBe "here you go"
                val fc = parts[1].functionCall.shouldNotBeNull()
                fc.name shouldBe "get_weather"
                fc.args["city"] shouldBe "Paris"

                finalResponse("", emptyList(), "gemma4").content.shouldNotBeNull().parts.size shouldBe 0
            }
        }
    }

    Given("a tool round-trip history") {
        When("flattening to Ollama messages") {
            Then("system, user, assistant tool-call and tool-result messages are produced") {
                val req =
                    LlmRequest(
                        config = GenerateContentConfig(systemInstruction = userText("sys")),
                        contents =
                            listOf(
                                Content(role = Role.USER, parts = listOf(Part(text = "read a.go"))),
                                Content(role = Role.MODEL, parts = listOf(Part(functionCall = FunctionCall(name = "read_file", args = mapOf("path" to "a.go"))))),
                                Content(role = Role.USER, parts = listOf(Part(functionResponse = FunctionResponse(name = "read_file", response = mapOf("content" to "package a"))))),
                            ),
                    )
                val msgs = toOllamaMessages(req)
                msgs.size shouldBe 4
                msgs[0].jsonObject.getValue("role").jsonPrimitive.content shouldBe "system"
                msgs[1].jsonObject.getValue("role").jsonPrimitive.content shouldBe "user"
                val asst = msgs[2].jsonObject
                asst.getValue("role").jsonPrimitive.content shouldBe "assistant"
                asst.getValue("tool_calls").jsonArray[0].jsonObject.getValue("function").jsonObject
                    .getValue("name").jsonPrimitive.content shouldBe "read_file"
                val toolMsg = msgs[3].jsonObject
                toolMsg.getValue("role").jsonPrimitive.content shouldBe "tool"
                toolMsg.getValue("tool_name").jsonPrimitive.content shouldBe "read_file"
                toolMsg.getValue("content").jsonPrimitive.content.contains("package a") shouldBe true
            }
        }
    }

    Given("a plain system/user/model history") {
        When("flattening to Ollama messages") {
            Then("each maps to the right role and content") {
                val req =
                    LlmRequest(
                        config = GenerateContentConfig(systemInstruction = userText("system rules")),
                        contents = listOf(userText("question"), assistantText("answer")),
                    )
                val msgs = toOllamaMessages(req)
                msgs.size shouldBe 3
                msgs.map { it.jsonObject.getValue("role").jsonPrimitive.content } shouldBe listOf("system", "user", "assistant")
                msgs.map { it.jsonObject.getValue("content").jsonPrimitive.content } shouldBe listOf("system rules", "question", "answer")
            }
        }
    }

    Given("an Ollama tool call") {
        When("converting to a genai function call") {
            Then("name and arguments are preserved") {
                val fc = toGenaiFunctionCall(OllamaToolCall(OllamaToolCallFunction(name = "f", arguments = buildJsonObject { put("x", "y") })))
                fc.name shouldBe "f"
                fc.args["x"] shouldBe "y"
            }
        }
    }

    Given("the jsonString helper") {
        When("serializing maps") {
            Then("null yields empty and a map yields compact JSON") {
                jsonString(null) shouldBe ""
                jsonString(mapOf("a" to "b")) shouldBe """{"a":"b"}"""
            }
        }
    }
})
