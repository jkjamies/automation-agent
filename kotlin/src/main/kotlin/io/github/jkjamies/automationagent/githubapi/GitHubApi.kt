/*
 * Package githubapi wraps the GitHub REST API with the narrow operations this service needs:
 * reading recent commits, opening/labeling/finding agent PRs, and reading the agent verify check.
 * Deterministic tooling — no agent imports.
 *
 * The base URL is injectable so tests can point the client at a Ktor MockEngine.
 */
package io.github.jkjamies.automationagent.githubapi

import io.ktor.client.HttpClient
import io.ktor.client.call.body
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.plugins.defaultRequest
import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.client.request.post
import io.ktor.client.request.setBody
import io.ktor.client.statement.HttpResponse
import io.ktor.client.statement.bodyAsText
import io.ktor.http.ContentType
import io.ktor.http.HttpHeaders
import io.ktor.http.contentType
import io.ktor.http.isSuccess
import io.ktor.serialization.kotlinx.json.json
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.json.Json
import java.io.IOException
import java.net.URLEncoder
import java.nio.charset.StandardCharsets
import java.time.Instant
import java.time.format.DateTimeFormatter
import java.util.Base64

// --- Public projections ---

/** A minimal commit projection for digests. */
data class Commit(
    val sha: String,
    val message: String,
    val author: String,
    val url: String,
    val at: Instant, // author date
)

/** A minimal pull-request projection. */
data class Pr(
    val number: Int,
    val title: String,
    val branch: String,
    val headSha: String,
    val url: String,
    val labels: List<String>,
)

/** Describes a pull request to open. */
data class PrInput(
    val title: String,
    val head: String, // source branch
    val base: String, // target branch
    val body: String = "",
)

/** The agent verify check's state for a ref. */
data class CheckResult(
    val found: Boolean,
    val name: String = "",
    val status: String = "", // queued | in_progress | completed
    val conclusion: String = "", // success | failure | ... (when completed)
    val outputText: String = "", // the check's output (lint findings), used to re-triage
    val startedAt: Instant = Instant.EPOCH,
    val completedAt: Instant = Instant.EPOCH,
)

/** Summarizes what changed between two refs (base...head). */
data class Comparison(val totalCommits: Int = 0, val files: List<ChangedFile> = emptyList())

/** One file in a [Comparison]. */
data class ChangedFile(val path: String, val status: String = "", val additions: Int = 0, val deletions: Int = 0)

/** The parsed essentials of a GitHub check_run webhook event. */
data class CheckEvent(
    val action: String, // created | completed | rerequested
    val checkName: String,
    val status: String,
    val conclusion: String,
    val headSha: String,
    val prNumber: Int,
    val prBranch: String,
    val repoFullName: String, // owner/name
    val outputText: String,
)

/**
 * A thin wrapper over the GitHub REST API. Owner/repo are passed per call so one client
 * serves many repositories. An empty [token] yields unauthenticated requests (fine for
 * public reads and tests).
 */
class Client(
    private val token: String = "",
    private val baseUrl: String = "https://api.github.com/",
    httpClient: HttpClient? = null,
) {
    private val http: HttpClient = httpClient ?: HttpClient(CIO) {
        install(ContentNegotiation) { json(githubJson) }
        install(HttpTimeout) {
            requestTimeoutMillis = REQUEST_TIMEOUT_MS
            connectTimeoutMillis = CONNECT_TIMEOUT_MS
            socketTimeoutMillis = SOCKET_TIMEOUT_MS
        }
        defaultRequest {
            header(HttpHeaders.Accept, "application/vnd.github+json")
            if (token.isNotEmpty()) header(HttpHeaders.Authorization, "Bearer $token")
        }
    }

    /** Returns commits to owner/repo authored since the given time. */
    suspend fun listCommitsSince(owner: String, repo: String, since: Instant): List<Commit> {
        var url: String? = url(
            "repos/$owner/$repo/commits",
            "since" to DateTimeFormatter.ISO_INSTANT.format(since),
            "per_page" to "100",
        )
        val out = mutableListOf<Commit>()
        while (url != null) {
            val resp = http.get(url).orThrow()
            resp.body<List<CommitDto>>().forEach { out += it.toCommit() }
            url = resp.nextLink()
        }
        return out
    }

    /** Opens a pull request. */
    suspend fun createPr(owner: String, repo: String, input: PrInput): Pr {
        val resp = http.post(url("repos/$owner/$repo/pulls")) {
            contentType(ContentType.Application.Json)
            setBody(NewPullRequestDto(input.title, input.head, input.base, input.body))
        }.orThrow()
        return resp.body<PrDto>().toPr()
    }

    /** Adds labels to a PR (PRs are issues for the labels API). */
    suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) {
        http.post(url("repos/$owner/$repo/issues/$number/labels")) {
            contentType(ContentType.Application.Json)
            setBody(labels)
        }.orThrow()
    }

    /**
     * Returns the open PR whose head is the given branch, or null. Lookup is by branch (the GitHub
     * `head=owner:branch` filter), not the agent label — the label is write-only, applied on
     * creation for humans to filter on.
     */
    suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String): Pr? {
        val resp = http.get(
            url("repos/$owner/$repo/pulls", "state" to "open", "head" to "$owner:$branch", "per_page" to "1"),
        ).orThrow()
        return resp.body<List<PrDto>>().firstOrNull()?.toPr()
    }

    /** Returns the named check's state for ref, or found=false if absent. */
    suspend fun agentCheck(owner: String, repo: String, ref: String, checkName: String): CheckResult {
        val resp =
            http.get(
                // filter=latest: on a re-run, return only the most recent run per check name, so we
                // never read a stale prior run (matches the Go reference's Filter: ptr("latest")).
                url("repos/$owner/$repo/commits/$ref/check-runs", "check_name" to checkName, "filter" to "latest"),
            ).orThrow()
        val dto = resp.body<CheckRunsDto>()
        if (dto.totalCount == 0 || dto.checkRuns.isEmpty()) return CheckResult(found = false)
        val cr = dto.checkRuns[0]
        return CheckResult(
            found = true,
            name = cr.name.orEmpty(),
            status = cr.status.orEmpty(),
            conclusion = cr.conclusion.orEmpty(),
            outputText = cr.output.text(),
            startedAt = parseInstant(cr.startedAt),
            completedAt = parseInstant(cr.completedAt),
        )
    }

    /**
     * Returns the commits and files changed between base and head (base...head). It is how a
     * terminal summary reports what the agent actually did across its attempts, since the per-attempt
     * work product lives only in the PR, not the session.
     */
    suspend fun compare(owner: String, repo: String, base: String, head: String): Comparison {
        val resp = http.get(url("repos/$owner/$repo/compare/$base...$head")).orThrow()
        val dto = resp.body<CompareDto>()
        return Comparison(
            totalCommits = dto.totalCommits,
            files = dto.files.map { ChangedFile(path = it.filename.orEmpty(), status = it.status.orEmpty(), additions = it.additions, deletions = it.deletions) },
        )
    }

    /**
     * Returns the decoded contents of a file at ref (ref may be "" for the default branch).
     */
    suspend fun getFileContent(owner: String, repo: String, path: String, ref: String): String {
        val query = if (ref.isEmpty()) emptyArray() else arrayOf("ref" to ref)
        val resp = http.get(url("repos/$owner/$repo/contents/$path", *query)).orThrow()
        val dto = resp.body<ContentsDto>()
        val cleaned = dto.content.orEmpty().replace("\n", "").replace("\r", "")
        return String(Base64.getDecoder().decode(cleaned))
    }

    private suspend fun HttpResponse.orThrow(): HttpResponse {
        if (!status.isSuccess()) throw IOException("github ${status.value}: ${bodyAsText().take(512)}")
        return this
    }

    private fun url(path: String, vararg query: Pair<String, String>): String {
        val base = baseUrl.trimEnd('/')
        if (query.isEmpty()) return "$base/$path"
        val q = query.joinToString("&") { (k, v) ->
            "${URLEncoder.encode(k, StandardCharsets.UTF_8)}=${URLEncoder.encode(v, StandardCharsets.UTF_8)}"
        }
        return "$base/$path?$q"
    }

    private fun HttpResponse.nextLink(): String? {
        val link = headers["Link"] ?: return null
        return link.split(",").firstNotNullOfOrNull { part ->
            val segs = part.split(";").map { it.trim() }
            val href = segs.firstOrNull()?.removePrefix("<")?.removeSuffix(">")
            if (href != null && segs.drop(1).any { it == "rel=\"next\"" }) href else null
        }
    }

    companion object {
        private const val REQUEST_TIMEOUT_MS = 30_000L
        private const val CONNECT_TIMEOUT_MS = 10_000L
        private const val SOCKET_TIMEOUT_MS = 30_000L

        /** Parses a check_run webhook body. */
        fun parseCheckRunEvent(body: ByteArray): CheckEvent = parseCheckRunEvent(String(body))

        fun parseCheckRunEvent(body: String): CheckEvent {
            val ev = githubJson.decodeFromString<CheckRunEventDto>(body)
            val cr = ev.checkRun
            val firstPr = cr?.pullRequests?.firstOrNull()
            return CheckEvent(
                action = ev.action.orEmpty(),
                checkName = cr?.name.orEmpty(),
                status = cr?.status.orEmpty(),
                conclusion = cr?.conclusion.orEmpty(),
                headSha = cr?.headSha.orEmpty(),
                prNumber = firstPr?.number ?: 0,
                prBranch = firstPr?.head?.ref.orEmpty(),
                repoFullName = ev.repository?.fullName.orEmpty(),
                outputText = cr?.output.text(),
            )
        }
    }
}

private val githubJson = Json { ignoreUnknownKeys = true; encodeDefaults = true }

private fun parseInstant(s: String?): Instant =
    if (s.isNullOrEmpty()) Instant.EPOCH else runCatching { Instant.parse(s) }.getOrDefault(Instant.EPOCH)

/** Returns the check output text, falling back to the summary. */
private fun OutputDto?.text(): String {
    if (this == null) return ""
    return text.orEmpty().ifEmpty { summary.orEmpty() }
}

// --- Serialization DTOs (GitHub wire shapes) ---

@Serializable
private data class CommitDto(
    val sha: String? = null,
    @SerialName("html_url") val htmlUrl: String? = null,
    val commit: CommitInnerDto? = null,
) {
    fun toCommit() = Commit(
        sha = sha.orEmpty(),
        message = commit?.message.orEmpty(),
        author = commit?.author?.name.orEmpty(),
        url = htmlUrl.orEmpty(),
        at = parseInstant(commit?.author?.date),
    )
}

@Serializable
private data class CommitInnerDto(val message: String? = null, val author: GitUserDto? = null)

@Serializable
private data class GitUserDto(val name: String? = null, val date: String? = null)

@Serializable
private data class PrDto(
    val number: Int? = null,
    val title: String? = null,
    @SerialName("html_url") val htmlUrl: String? = null,
    val head: RefDto? = null,
    val labels: List<LabelDto> = emptyList(),
) {
    fun toPr() = Pr(
        number = number ?: 0,
        title = title.orEmpty(),
        branch = head?.ref.orEmpty(),
        headSha = head?.sha.orEmpty(),
        url = htmlUrl.orEmpty(),
        labels = labels.map { it.name.orEmpty() },
    )
}

@Serializable
private data class RefDto(val ref: String? = null, val sha: String? = null)

@Serializable
private data class LabelDto(val name: String? = null)

@Serializable
private data class CheckRunsDto(
    @SerialName("total_count") val totalCount: Int = 0,
    @SerialName("check_runs") val checkRuns: List<CheckRunDto> = emptyList(),
)

@Serializable
private data class CheckRunDto(
    val name: String? = null,
    val status: String? = null,
    val conclusion: String? = null,
    @SerialName("head_sha") val headSha: String? = null,
    @SerialName("started_at") val startedAt: String? = null,
    @SerialName("completed_at") val completedAt: String? = null,
    val output: OutputDto? = null,
    @SerialName("pull_requests") val pullRequests: List<PrRefDto> = emptyList(),
)

@Serializable
private data class OutputDto(val text: String? = null, val summary: String? = null)

@Serializable
private data class PrRefDto(val number: Int? = null, val head: RefDto? = null)

@Serializable
private data class ContentsDto(
    val type: String? = null,
    val encoding: String? = null,
    val content: String? = null,
    val path: String? = null,
)

@Serializable
private data class NewPullRequestDto(
    val title: String,
    val head: String,
    val base: String,
    val body: String,
)

@Serializable
private data class CheckRunEventDto(
    val action: String? = null,
    @SerialName("check_run") val checkRun: CheckRunDto? = null,
    val repository: RepoDto? = null,
)

@Serializable
private data class RepoDto(@SerialName("full_name") val fullName: String? = null)

@Serializable
private data class CompareDto(
    @SerialName("total_commits") val totalCommits: Int = 0,
    val files: List<CompareFileDto> = emptyList(),
)

@Serializable
private data class CompareFileDto(
    val filename: String? = null,
    val status: String? = null,
    val additions: Int = 0,
    val deletions: Int = 0,
)
