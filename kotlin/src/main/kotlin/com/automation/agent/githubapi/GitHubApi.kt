/*
 * Package githubapi wraps the GitHub REST API with the narrow operations this service needs:
 * reading recent commits, opening/labeling/finding agent PRs, and reading the agent verify check.
 * Deterministic tooling — no agent imports.
 *
 * The base URL is injectable so tests can point the client at a Ktor MockEngine.
 */
package com.automation.agent.githubapi

import io.ktor.client.HttpClient
import io.ktor.client.call.body
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.plugins.defaultRequest
import io.ktor.client.request.HttpRequestPipeline
import io.ktor.client.request.get
import io.ktor.client.request.header
import io.ktor.client.request.patch
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
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.add
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
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
 * One changed file in a pull request: its path, change status, line counts, and the unified diff
 * patch. [patch] carries the hunk text the reviewer needs to map a finding to a diff line; GitHub
 * omits it for binary or very large files, so it is then empty — kept, not an error. Because an empty
 * patch is ambiguous (binary vs. oversized text), [additions]/[deletions] are reported even when the
 * patch is omitted, letting an omitted text diff be charged conservatively from its line counts
 * rather than as zero diff bytes.
 */
data class PRFile(
    val path: String,
    val previousPath: String = "", // prior path for a rename, else empty
    val status: String = "", // added | modified | removed | renamed | copied | changed
    val additions: Int = 0,
    val deletions: Int = 0,
    val patch: String = "", // unified diff hunks; empty for binary/oversized files
)

/**
 * The parsed essentials of a GitHub pull_request webhook event — the reviewer's native-event
 * kickoff. The diff itself is fetched separately via [Client.listPRFiles] (the event body carries
 * only metadata).
 */
data class PullRequestEvent(
    val action: String, // opened | reopened | synchronize | ready_for_review | ...
    val number: Int,
    val repoFullName: String, // owner/name
    val headRef: String, // source branch
    val headSha: String,
    val baseRef: String, // target branch
    val draft: Boolean,
    val labels: List<String>,
    val authorLogin: String, // PR author login (e.g. "dependabot[bot]")
)

/** One entry in a repository git tree: its repo-relative path, blob/tree SHA, and type. */
data class TreeEntry(val path: String, val sha: String, val type: String) // "blob" | "tree"

/** A repository git tree listing plus GitHub's truncation flag (a capped recursive tree may omit entries). */
data class Tree(val entries: List<TreeEntry>, val truncated: Boolean)

/**
 * Identifies an existing inline review comment for reconciliation: its GraphQL node id (the
 * minimize-comment subject) and its body (which carries the hidden fingerprint marker).
 */
data class ReviewCommentRef(val nodeId: String, val body: String)

/**
 * One inline review comment on the head (RIGHT) side of a file. GitHub rejects an inline comment
 * whose line is outside the PR's diff hunks, so the caller posts only in-diff findings here and
 * lists the rest in the summary comment.
 */
data class ReviewComment(val path: String, val line: Int, val side: String, val body: String)

/**
 * An advisory pull-request review: a body plus optional inline comments. The reviewer never
 * approves or requests changes, so the event is always COMMENT.
 */
data class ReviewInput(val body: String = "", val comments: List<ReviewComment> = emptyList())

/**
 * Describes the advisory agent-review check run: always completed, conclusion success or neutral —
 * never failure, so it informs without gating merges.
 */
data class CheckRunInput(
    val name: String,
    val headSha: String,
    val conclusion: String, // "success" | "neutral"
    val title: String = "",
    val summary: String = "",
)

/** Yields a currently-valid GitHub token for the REST client, or `""` for anonymous requests. The
 * githubapi-local view of the `auth.TokenProvider` seam (a narrow interface kept here so githubapi
 * stays decoupled from `auth`; the composition root adapts the real provider to it). */
fun interface TokenSource {
    suspend fun token(): String
}

/**
 * A thin wrapper over the GitHub REST API. Owner/repo are passed per call so one client
 * serves many repositories. A null [tokenSource] (or one yielding `""`) leaves requests
 * unauthenticated (fine for public reads and tests).
 */
class Client(
    private val tokenSource: TokenSource? = null,
    private val baseUrl: String = "https://api.github.com/",
    httpClient: HttpClient? = null,
    // The login this client authors content as ("<slug>[bot]" in App mode, the user login in PAT
    // mode), resolved by the composition root and injected. "" means it could not be resolved, in
    // which case appAuthored picks a safe fallback for marker-comment ownership (see ownsComment).
    private val authoredLogin: String = "",
    // True when the REST token comes from a GitHub App installation, so an unresolved identity can
    // still fall back to trusting only bot-authored comments (see ownsComment).
    private val appAuthored: Boolean = false,
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
        }
    }

    init {
        // Inject a fresh bearer token per request from the provider seam (the analogue of the Go
        // reference's token-injecting RoundTripper). The provider caches/refreshes a short-lived App
        // installation token, so this stays cheap; an empty token leaves the request anonymous.
        tokenSource?.let { source ->
            http.requestPipeline.intercept(HttpRequestPipeline.State) {
                val token = source.token()
                if (token.isNotEmpty()) context.header(HttpHeaders.Authorization, "Bearer $token")
            }
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

    /**
     * Returns every changed file in a pull request (following pagination). It is the reviewer's
     * primary input — changed files + patches — fetched via REST.
     */
    suspend fun listPRFiles(owner: String, repo: String, num: Int): List<PRFile> {
        var url: String? = url("repos/$owner/$repo/pulls/$num/files", "per_page" to "100")
        val out = mutableListOf<PRFile>()
        while (url != null) {
            val resp = http.get(url).orThrow()
            resp.body<List<PRFileDto>>().forEach { out += it.toPRFile() }
            url = resp.nextLink()
        }
        return out
    }

    /**
     * Returns the PR's current head commit SHA. The reviewer compares it to the SHA carried by a
     * review task to detect a task superseded by a newer push and skip a stale review.
     */
    suspend fun pullRequestHeadSha(owner: String, repo: String, num: Int): String {
        val resp = http.get(url("repos/$owner/$repo/pulls/$num")).orThrow()
        return resp.body<PrDto>().head?.sha.orEmpty()
    }

    /**
     * Lists the repository's git tree at [ref] (a commit SHA, branch, or tag), recursively — how the
     * reviewer discovers a repo's own standards docs without a clone. The [Tree.truncated] flag is
     * GitHub's: the API caps a recursive tree (very large repos), and a truncated listing may omit
     * entries, so the caller can decide whether incomplete discovery is acceptable rather than
     * silently missing files.
     */
    suspend fun tree(owner: String, repo: String, ref: String): Tree {
        val resp = http.get(url("repos/$owner/$repo/git/trees/$ref", "recursive" to "true")).orThrow()
        val dto = resp.body<TreeDto>()
        val entries = dto.tree.map { TreeEntry(path = it.path.orEmpty(), sha = it.sha.orEmpty(), type = it.type.orEmpty()) }
        return Tree(entries = entries, truncated = dto.truncated)
    }

    /**
     * Returns the PR's inline review comments (paginated). Reconciliation parses the fingerprint
     * marker from each body to decide what to keep, add, or minimize.
     */
    suspend fun listReviewComments(owner: String, repo: String, num: Int): List<ReviewCommentRef> {
        var url: String? = url("repos/$owner/$repo/pulls/$num/comments", "per_page" to "100")
        val out = mutableListOf<ReviewCommentRef>()
        while (url != null) {
            val resp = http.get(url).orThrow()
            resp.body<List<ReviewCommentDto>>().forEach { out += ReviewCommentRef(nodeId = it.nodeId.orEmpty(), body = it.body.orEmpty()) }
            url = resp.nextLink()
        }
        return out
    }

    /** Posts an advisory (COMMENT) pull-request review with optional inline comments. */
    suspend fun createReview(owner: String, repo: String, num: Int, input: ReviewInput) {
        val payload =
            buildJsonObject {
                put("event", "COMMENT")
                if (input.body.isNotEmpty()) put("body", input.body)
                put(
                    "comments",
                    buildJsonArray {
                        input.comments.forEach { c ->
                            add(
                                buildJsonObject {
                                    put("path", c.path)
                                    put("body", c.body)
                                    put("line", c.line)
                                    put("side", c.side)
                                },
                            )
                        }
                    },
                )
            }
        http.post(url("repos/$owner/$repo/pulls/$num/reviews")) {
            contentType(ContentType.Application.Json)
            setBody(githubJson.encodeToString(JsonObject.serializer(), payload))
        }.orThrow()
    }

    /**
     * Posts a completed, advisory check run for the head SHA. The agent-review check is advisory and
     * must never gate a merge, so the conclusion is constrained here at the API boundary — a
     * "failure"/"cancelled" cannot slip in.
     */
    suspend fun createCheckRun(owner: String, repo: String, input: CheckRunInput) {
        if (input.conclusion != "success" && input.conclusion != "neutral") {
            throw IllegalArgumentException("create check run $owner/$repo: advisory conclusion must be success or neutral, got \"${input.conclusion}\"")
        }
        val payload =
            buildJsonObject {
                put("name", input.name)
                put("head_sha", input.headSha)
                put("status", "completed")
                put("conclusion", input.conclusion)
                put("output", buildJsonObject { put("title", input.title); put("summary", input.summary) })
            }
        http.post(url("repos/$owner/$repo/check-runs")) {
            contentType(ContentType.Application.Json)
            setBody(githubJson.encodeToString(JsonObject.serializer(), payload))
        }.orThrow()
    }

    /**
     * Collapses a comment as OUTDATED via GraphQL (the REST API has no equivalent), so a finding that
     * no longer applies is hidden rather than deleted — the thread is preserved. [subjectId] is the
     * comment's GraphQL node id ([ReviewCommentRef.nodeId]). The mutation runs over the same
     * authenticated client as REST; the endpoint derives from the REST base incl. the GitHub
     * Enterprise Server `/api/v3` -> `/api/graphql` mapping.
     */
    suspend fun minimizeComment(subjectId: String) {
        val mutation = "mutation(\$id:ID!){minimizeComment(input:{subjectId:\$id,classifier:OUTDATED}){minimizedComment{isMinimized}}}"
        val payload =
            buildJsonObject {
                put("query", mutation)
                put("variables", buildJsonObject { put("id", subjectId) })
            }
        val resp =
            http.post(graphqlUrl()) {
                contentType(ContentType.Application.Json)
                setBody(githubJson.encodeToString(JsonObject.serializer(), payload))
            }
        if (!resp.status.isSuccess()) throw IOException("graphql: unexpected status ${resp.status.value}")
        val decoded = githubJson.decodeFromString<GraphQlResponseDto>(resp.bodyAsText())
        if (decoded.errors.isNotEmpty()) throw IOException("graphql: ${decoded.errors[0].message}")
    }

    /**
     * Edits the single issue comment this client authored whose body contains [marker], or creates
     * one if none exists. The reviewer's summary comment carries a hidden marker so a re-review
     * updates it in place instead of piling up duplicates. Only a comment the client could have
     * authored is edited (see [ownsComment]): GitHub rejects editing a foreign comment, so a comment
     * that merely echoes the marker must not hijack the upsert.
     */
    suspend fun upsertMarkerComment(owner: String, repo: String, num: Int, marker: String, body: String) {
        // An empty marker would match every comment and edit an unrelated one; a body without the
        // marker could never be found again, piling up duplicates. Both are caller bugs, so fail fast.
        if (marker.isEmpty()) throw IllegalArgumentException("upsert comment $owner/$repo#$num: empty marker")
        if (!body.contains(marker)) throw IllegalArgumentException("upsert comment $owner/$repo#$num: body must contain the marker")
        val editPayload = githubJson.encodeToString(JsonObject.serializer(), buildJsonObject { put("body", body) })
        var next: String? = url("repos/$owner/$repo/issues/$num/comments", "per_page" to "100")
        while (next != null) {
            val resp = http.get(next).orThrow()
            for (ic in resp.body<List<IssueCommentDto>>()) {
                if (!(ic.body ?: "").contains(marker) || !ownsComment(ic)) continue
                val id = ic.id ?: continue
                val editResp =
                    http.patch(url("repos/$owner/$repo/issues/comments/$id")) {
                        contentType(ContentType.Application.Json)
                        setBody(editPayload)
                    }
                if (editResp.status.isSuccess()) return
                // With a known login the match is authoritative, so any edit failure is a real error.
                // On the weak author-type fallback (identity unresolved) the match can be a foreign
                // bot that merely echoes the marker; a 403/404 there means "not ours", so skip it and
                // fall through to create.
                if (authoredLogin == "" && (editResp.status.value == 403 || editResp.status.value == 404)) continue
                throw IOException("edit comment $owner/$repo#$num: github ${editResp.status.value}: ${editResp.bodyAsText().take(512)}")
            }
            next = resp.nextLink()
        }
        http.post(url("repos/$owner/$repo/issues/$num/comments")) {
            contentType(ContentType.Application.Json)
            setBody(editPayload)
        }.orThrow()
    }

    /**
     * Reports whether this client authored [ic] — the precondition for editing it in place (GitHub
     * rejects editing a comment the client did not author). A known login is the authoritative check
     * (byte-for-byte match); otherwise fall back to author type: App mode trusts only bot-authored
     * comments; PAT/anonymous trusts the marker alone.
     */
    private fun ownsComment(ic: IssueCommentDto): Boolean {
        if (authoredLogin != "") return (ic.user?.login ?: "") == authoredLogin
        if (appAuthored) return (ic.user?.type ?: "") == "Bot"
        return true
    }

    /**
     * Derives the GraphQL endpoint from the REST base. api.github.com's REST base yields
     * `.../graphql`; GitHub Enterprise Server serves REST at `<host>/api/v3` but GraphQL at
     * `<host>/api/graphql`, so that path is mapped explicitly.
     */
    private fun graphqlUrl(): String {
        val base = baseUrl.trimEnd('/')
        if (base.endsWith("/api/v3")) return base.removeSuffix("/v3") + "/graphql"
        return "$base/graphql"
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

/** Parses a pull_request webhook body into the fields the reviewer gates on. */
fun parsePullRequestEvent(body: ByteArray): PullRequestEvent = parsePullRequestEvent(String(body))

/**
 * Parses a pull_request webhook body into a [PullRequestEvent]. Missing fields degrade to empty/0
 * defaults; invalid JSON is an [IllegalArgumentException]. The webhook JSON is decoded in the tooling
 * layer so the agent consumes a stable projection, never the raw wire type.
 */
fun parsePullRequestEvent(body: String): PullRequestEvent {
    val ev =
        try {
            githubJson.decodeFromString<PullRequestEventDto>(body)
        } catch (e: Exception) {
            throw IllegalArgumentException("parse pull_request event: ${e.message}", e)
        }
    val pr = ev.pullRequest
    return PullRequestEvent(
        action = ev.action.orEmpty(),
        number = pr?.number ?: 0,
        repoFullName = ev.repository?.fullName.orEmpty(),
        headRef = pr?.head?.ref.orEmpty(),
        headSha = pr?.head?.sha.orEmpty(),
        baseRef = pr?.base?.ref.orEmpty(),
        draft = pr?.draft ?: false,
        labels = pr?.labels?.mapNotNull { it.name?.ifEmpty { null } } ?: emptyList(),
        authorLogin = pr?.user?.login.orEmpty(),
    )
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

@Serializable
private data class PRFileDto(
    val filename: String? = null,
    @SerialName("previous_filename") val previousFilename: String? = null,
    val status: String? = null,
    val additions: Int = 0,
    val deletions: Int = 0,
    val patch: String? = null,
) {
    fun toPRFile() = PRFile(
        path = filename.orEmpty(),
        previousPath = previousFilename.orEmpty(),
        status = status.orEmpty(),
        additions = additions,
        deletions = deletions,
        patch = patch.orEmpty(),
    )
}

@Serializable
private data class PullRequestEventDto(
    val action: String? = null,
    @SerialName("pull_request") val pullRequest: PullRequestDto? = null,
    val repository: RepoDto? = null,
)

@Serializable
private data class PullRequestDto(
    val number: Int? = null,
    val head: RefDto? = null,
    val base: RefDto? = null,
    val draft: Boolean = false,
    val labels: List<LabelDto> = emptyList(),
    val user: UserDto? = null,
)

@Serializable
private data class UserDto(val login: String? = null)

@Serializable
private data class TreeDto(
    val tree: List<TreeEntryDto> = emptyList(),
    val truncated: Boolean = false,
)

@Serializable
private data class TreeEntryDto(val path: String? = null, val sha: String? = null, val type: String? = null)

@Serializable
private data class ReviewCommentDto(
    @SerialName("node_id") val nodeId: String? = null,
    val body: String? = null,
)

@Serializable
private data class IssueCommentDto(
    val id: Long? = null,
    val body: String? = null,
    val user: IssueUserDto? = null,
)

@Serializable
private data class IssueUserDto(val login: String? = null, val type: String? = null)

@Serializable
private data class GraphQlResponseDto(val errors: List<GraphQlErrorDto> = emptyList())

@Serializable
private data class GraphQlErrorDto(val message: String = "")
