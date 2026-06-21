package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.models.LlmRequest
import com.google.adk.kt.models.LlmResponse
import com.google.adk.kt.models.Model
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FinishReason
import com.google.adk.kt.types.FunctionCall
import com.google.adk.kt.types.GenerateContentConfig
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Role
import com.google.adk.kt.types.Schema
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.request.post
import io.ktor.client.request.setBody
import io.ktor.client.statement.bodyAsText
import io.ktor.http.ContentType
import io.ktor.http.contentType
import io.ktor.http.isSuccess
import io.ktor.serialization.kotlinx.json.json
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.add
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.longOrNull
import kotlinx.serialization.json.put
import java.io.IOException

/**
 * `defaultNumCtx` is the context window requested from Ollama. Gemma is served with a 32k window;
 * setting it avoids the server default (~4k) that would silently chop large file prompts.
 */
private const val DEFAULT_NUM_CTX = 32768

/** Bounds how long the client waits to establish a TCP connection to the Ollama server. */
private const val OLLAMA_CONNECT_TIMEOUT_MS = 10_000L

private val ollamaJson = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }

/**
 * OllamaModel adapts a local Ollama server to the ADK-Kotlin [Model] interface so agents can run
 * against Gemma locally. It honors [GenerateContentConfig] (temperature, num_ctx, JSON format) and
 * tool declarations, so tool-using agents work locally.
 *
 * adk-kotlin ships no Ollama model, so this is the adapter path. There is no official Kotlin client,
 * so the thin `/api/chat` round-trip is implemented directly over Ktor (already a project
 * dependency). [httpClient] is injectable so tests point it at a Ktor `MockEngine`.
 */
class OllamaModel(
    host: String,
    modelTag: String,
    httpClient: HttpClient? = null,
) : Model {
    init {
        require(modelTag.isNotEmpty()) { "ollama model tag must not be empty" }
    }

    /** Reports the configured model tag. */
    override val name: String = modelTag

    private val chatUrl: String = host.trimEnd('/') + "/api/chat"

    private val http: HttpClient =
        httpClient ?: HttpClient(CIO) {
            install(ContentNegotiation) { json(ollamaJson) }
            // Bound only the connect phase: request/socket timeouts are intentionally left open
            // because a local generation can legitimately stream for minutes.
            install(HttpTimeout) { connectTimeoutMillis = OLLAMA_CONNECT_TIMEOUT_MS }
        }

    /**
     * Implements [Model.generateContent]. It forwards generation options and tools, aggregates
     * streaming chunks, and surfaces tool calls as genai function-call parts. Errors (a non-2xx
     * response or transport failure) throw inside the flow.
     */
    override fun generateContent(request: LlmRequest, stream: Boolean): Flow<LlmResponse> = flow {
        val body =
            buildJsonObject {
                put("model", modelName(request))
                put("messages", toOllamaMessages(request))
                put("stream", stream)
                put("options", generationOptions(request.config))
                toOllamaTools(request.config)?.let { put("tools", it) }
                if (wantsJSON(request.config)) put("format", "json")
            }

        val response =
            http.post(chatUrl) {
                contentType(ContentType.Application.Json)
                setBody(body)
            }
        if (!response.status.isSuccess()) {
            throw IOException("ollama chat ${response.status.value}: ${response.bodyAsText().take(512)}")
        }

        // Ollama replies with newline-delimited JSON (one object per chunk; a single object when
        // stream=false). The body is read whole and replayed in order — partials are still emitted
        // before the final. (This service only ever calls stream=false; true token-by-token
        // delivery would matter for a live UI, which does not exist here.)
        val full = StringBuilder()
        val toolCalls = mutableListOf<OllamaToolCall>()
        for (line in response.bodyAsText().lineSequence()) {
            if (line.isBlank()) continue
            val chunk = ollamaJson.decodeFromString(OllamaChatResponse.serializer(), line)
            full.append(chunk.message.content)
            toolCalls += chunk.message.toolCalls

            if (stream && !chunk.done && chunk.message.content.isNotBlank()) {
                emit(newTextResponse(chunk.message.content, chunk.model).copy(partial = true))
            }
            if (chunk.done) {
                emit(finalResponse(full.toString(), toolCalls.toList(), chunk.model))
            }
        }
    }

    /** Prefers the request's model (which a before-model callback may set) over the default tag. */
    internal fun modelName(request: LlmRequest): String = request.model?.name ?: name
}

// --- Request construction (genai types -> Ollama wire JSON) ---

/**
 * Maps [GenerateContentConfig] onto Ollama options. Temperature defaults to 0 for deterministic
 * code/JSON; num_ctx is set so large files aren't truncated. (ADK-Kotlin's config has no `seed`
 * field, so seed is not forwarded.)
 */
internal fun generationOptions(config: GenerateContentConfig): JsonObject =
    buildJsonObject {
        put("num_ctx", DEFAULT_NUM_CTX)
        put("temperature", config.temperature?.toDouble() ?: 0.0)
        config.topP?.let { put("top_p", it.toDouble()) }
    }

internal fun wantsJSON(config: GenerateContentConfig): Boolean =
    config.responseMimeType?.contains("json", ignoreCase = true) == true

/** Converts genai function declarations into Ollama tool definitions, or null when there are none. */
internal fun toOllamaTools(config: GenerateContentConfig): JsonArray? {
    val tools = config.tools ?: return null
    val declarations = tools.flatMap { it.functionDeclarations ?: emptyList() }
    if (declarations.isEmpty()) return null
    return buildJsonArray {
        for (fd in declarations) {
            add(
                buildJsonObject {
                    put("type", "function")
                    put(
                        "function",
                        buildJsonObject {
                            put("name", fd.name)
                            put("description", fd.description)
                            put("parameters", toToolParams(fd.parameters))
                        },
                    )
                },
            )
        }
    }
}

internal fun toToolParams(schema: Schema?): JsonObject =
    buildJsonObject {
        put("type", schema?.type?.name?.lowercase() ?: "object")
        schema?.required?.takeIf { it.isNotEmpty() }?.let { req ->
            put("required", buildJsonArray { req.forEach { add(it) } })
        }
        val props = schema?.properties
        if (!props.isNullOrEmpty()) {
            put("properties", buildJsonObject { props.forEach { (n, ps) -> put(n, toToolProperty(ps)) } })
        }
    }

internal fun toToolProperty(schema: Schema?): JsonObject =
    buildJsonObject {
        if (schema == null) return@buildJsonObject
        schema.description?.let { put("description", it) }
        // Ollama property types are arrays (api.PropertyType is []string), lowercased.
        schema.type?.let { put("type", buildJsonArray { add(it.name.lowercase()) }) }
        schema.items?.let { put("items", toToolProperty(it)) }
        val props = schema.properties
        if (!props.isNullOrEmpty()) {
            put("properties", buildJsonObject { props.forEach { (n, ps) -> put(n, toToolProperty(ps)) } })
        }
    }

/**
 * Flattens the system instruction + contents into Ollama chat messages, including assistant
 * tool-calls and tool-result messages so the function-calling round-trip works.
 */
internal fun toOllamaMessages(request: LlmRequest): JsonArray =
    buildJsonArray {
        val sys = contentText(request.config.systemInstruction)
        if (sys.isNotEmpty()) {
            add(buildJsonObject { put("role", "system"); put("content", sys) })
        }
        for (c in request.contents) {
            val role = if (c.role == Role.MODEL) "assistant" else "user"
            val text = StringBuilder()
            val toolCalls = mutableListOf<JsonObject>()
            for (p in c.parts) {
                val fr = p.functionResponse
                val fc = p.functionCall
                val t = p.text
                when {
                    fr != null ->
                        add(
                            buildJsonObject {
                                put("role", "tool")
                                put("tool_name", fr.name)
                                put("content", jsonString(fr.response))
                            },
                        )
                    fc != null -> toolCalls += toOllamaToolCall(fc)
                    t != null -> text.append(t)
                }
            }
            if (text.isNotEmpty() || toolCalls.isNotEmpty()) {
                add(
                    buildJsonObject {
                        put("role", role)
                        put("content", text.toString())
                        if (toolCalls.isNotEmpty()) put("tool_calls", buildJsonArray { toolCalls.forEach { add(it) } })
                    },
                )
            }
        }
    }

internal fun toOllamaToolCall(fc: FunctionCall): JsonObject =
    buildJsonObject {
        put(
            "function",
            buildJsonObject {
                put("name", fc.name)
                put("arguments", mapToJsonObject(fc.args))
            },
        )
    }

/** Serializes a response map to compact JSON; "" when null. */
internal fun jsonString(map: Map<String, Any?>?): String =
    if (map == null) "" else ollamaJson.encodeToString(JsonObject.serializer(), mapToJsonObject(map))

// --- Response construction (Ollama wire JSON -> genai LlmResponse) ---

internal fun newTextResponse(text: String, modelVersion: String): LlmResponse =
    LlmResponse(content = Content(role = Role.MODEL, parts = listOf(Part(text = text))), modelVersion = modelVersion)

/**
 * Builds the terminal response, including any tool calls as genai function-call parts so the runner
 * can execute the tools. ADK-Kotlin's [LlmResponse] has no `turnComplete` flag (that lives on the
 * downstream `Event`); the terminal response is marked by `finishReason = STOP` and `partial=false`.
 */
internal fun finalResponse(text: String, toolCalls: List<OllamaToolCall>, modelVersion: String): LlmResponse {
    val parts =
        buildList {
            if (text.isNotBlank()) add(Part(text = text))
            toolCalls.forEach { add(Part(functionCall = toGenaiFunctionCall(it))) }
        }
    return LlmResponse(
        content = Content(role = Role.MODEL, parts = parts),
        modelVersion = modelVersion,
        finishReason = FinishReason.STOP,
    )
}

internal fun toGenaiFunctionCall(tc: OllamaToolCall): FunctionCall {
    // Ollama tool calls carry no id; key correlation on the name as a fallback. When the id is
    // still absent downstream, ADK-Kotlin's Event.populateClientFunctionCallId() generates one.
    val name = tc.function.name
    return FunctionCall(name = name, args = jsonObjectToMap(tc.function.arguments), id = name)
}

// --- JSON <-> Any helpers ---

internal fun mapToJsonObject(map: Map<String, Any?>): JsonObject =
    buildJsonObject { map.forEach { (k, v) -> put(k, anyToJson(v)) } }

private fun anyToJson(v: Any?): JsonElement =
    when (v) {
        null -> JsonNull
        is JsonElement -> v
        is String -> JsonPrimitive(v)
        is Boolean -> JsonPrimitive(v)
        is Number -> JsonPrimitive(v)
        is Map<*, *> -> buildJsonObject { v.forEach { (k, vv) -> put(k.toString(), anyToJson(vv)) } }
        is Iterable<*> -> buildJsonArray { v.forEach { add(anyToJson(it)) } }
        else -> JsonPrimitive(v.toString())
    }

private fun jsonObjectToMap(obj: JsonObject): Map<String, Any?> = obj.mapValues { (_, v) -> jsonElementToAny(v) }

private fun jsonElementToAny(el: JsonElement): Any? =
    when (el) {
        is JsonNull -> null
        is JsonObject -> el.mapValues { (_, v) -> jsonElementToAny(v) }
        is JsonArray -> el.map { jsonElementToAny(it) }
        is JsonPrimitive ->
            when {
                el.isString -> el.content
                el.booleanOrNull != null -> el.boolean
                el.longOrNull != null -> el.longOrNull
                el.intOrNull != null -> el.intOrNull
                el.doubleOrNull != null -> el.doubleOrNull
                else -> el.content
            }
    }

// --- Ollama wire shapes (response) ---

@Serializable
internal data class OllamaChatResponse(
    val model: String = "",
    val message: OllamaRespMessage = OllamaRespMessage(),
    val done: Boolean = false,
    @SerialName("done_reason") val doneReason: String? = null,
)

@Serializable
internal data class OllamaRespMessage(
    val role: String = "",
    val content: String = "",
    @SerialName("tool_calls") val toolCalls: List<OllamaToolCall> = emptyList(),
)

@Serializable
internal data class OllamaToolCall(val function: OllamaToolCallFunction = OllamaToolCallFunction())

@Serializable
internal data class OllamaToolCallFunction(
    val name: String = "",
    val arguments: JsonObject = JsonObject(emptyMap()),
)
