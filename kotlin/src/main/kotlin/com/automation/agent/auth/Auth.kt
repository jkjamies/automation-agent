/*
 * Package auth abstracts how the service authenticates to GitHub behind a single seam, the
 * [TokenProvider], so the rest of the code never sees whether a token came from a static PAT or a
 * freshly minted GitHub App installation token.
 *
 * Two providers implement the seam:
 *
 *   * [StaticProvider] — returns one constant token for every repo. Backs the PAT local-dev
 *     fallback (GITHUB_TOKEN / GH_TOKEN / `gh auth token`) and the empty, anonymous client used for
 *     public reads and tests.
 *   * [AppProvider] — mints and caches a short-lived (~1h), auto-refreshed installation token for a
 *     single pinned installation (single-org per deployment; see
 *     specs/20260625-github-app-authentication.md §1). The `repo` argument is accepted for the
 *     contract but ignored: one installation covers every repo in the deployment.
 *
 * Unlike the Go (ghinstallation), Python (PyGithub) and JS (@octokit/auth-app) ports, the JVM has
 * no off-the-shelf installation-token library, so [AppProvider] hand-rolls the App flow: it signs an
 * RS256 JWT with the App private key (java.security), exchanges it at
 * POST /app/installations/{id}/access_tokens for an installation token, and caches that token,
 * refreshing it shortly before expiry. The external contract (env vars, mode selection,
 * App-vs-PAT behavior) is identical across ports.
 *
 * Deterministic tooling — no agent imports.
 */
package com.automation.agent.auth

import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.request.header
import io.ktor.client.request.post
import io.ktor.client.statement.bodyAsText
import io.ktor.http.HttpHeaders
import io.ktor.http.isSuccess
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import org.bouncycastle.asn1.pkcs.PrivateKeyInfo
import org.bouncycastle.openssl.PEMKeyPair
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.openssl.jcajce.JcaPEMKeyConverter
import java.io.IOException
import java.io.StringReader
import java.security.PrivateKey
import java.security.Signature
import java.security.interfaces.RSAPrivateKey
import java.time.Instant
import java.util.Base64

/**
 * Yields a valid GitHub token for operations on a given repo. PAT mode returns the same constant for
 * every repo; App mode mints/caches a repo-scoped installation token and refreshes it before expiry.
 * [repo] is `"owner/name"`. An empty token means anonymous (public read only).
 *
 * The seam is the cross-port contract (`language-parity.md`); the minting library is per-port detail.
 */
interface TokenProvider {
    suspend fun token(repo: String): String
}

/**
 * Returns the same token for every repo. Backs PAT mode and the empty/anonymous client (an empty
 * token yields unauthenticated requests, fine for public reads and tests).
 */
class StaticProvider(private val token: String = "") : TokenProvider {
    /** Returns the constant token; [repo] is ignored. */
    override suspend fun token(repo: String): String = token
}

/**
 * Mints and caches a short-lived installation token for a single pinned installation. The JWT (RS256)
 * minting, the token exchange, and the caching/refresh are hand-rolled here (the JVM has no
 * ghinstallation/PyGithub/@octokit equivalent). The `repo` argument is accepted for the contract but
 * ignored: the installation is pinned and covers every repo in the deployment.
 *
 * Prefer [newAppProvider], which parses + validates the PEM into the [privateKey] this takes.
 */
class AppProvider(
    private val appId: Long,
    private val installationId: Long,
    private val privateKey: RSAPrivateKey,
    private val baseUrl: String = DEFAULT_BASE_URL,
    httpClient: HttpClient? = null,
    private val now: () -> Instant = Instant::now,
) : TokenProvider {
    private val http: HttpClient = httpClient ?: HttpClient(CIO) {
        install(HttpTimeout) {
            requestTimeoutMillis = EXCHANGE_TIMEOUT_MS
            connectTimeoutMillis = CONNECT_TIMEOUT_MS
        }
    }

    private val mutex = Mutex()

    @Volatile
    private var cached: Cached? = null

    /**
     * Returns a currently-valid installation token, minting on first call then refreshing shortly
     * before expiry. [repo] is ignored. The mutex serializes concurrent callers so a token is minted
     * at most once per refresh window (no thundering herd of exchanges).
     */
    override suspend fun token(repo: String): String = mutex.withLock {
        val nowAt = now()
        val current = cached
        if (current != null && nowAt.isBefore(current.expiresAt.minusSeconds(REFRESH_SKEW_SECONDS))) {
            return@withLock current.token
        }
        val fresh = exchange(nowAt)
        cached = fresh
        fresh.token
    }

    /** Signs an App JWT and exchanges it for an installation token at the pinned installation. */
    private suspend fun exchange(nowAt: Instant): Cached {
        val jwt = buildAppJwt(appId, privateKey, nowAt)
        val url = "${baseUrl.trimEnd('/')}/app/installations/$installationId/access_tokens"
        val resp = http.post(url) {
            // The App authenticates the exchange as itself with the RS256 JWT (not an installation
            // token yet); GitHub returns the installation token in the body.
            header(HttpHeaders.Authorization, "Bearer $jwt")
            header(HttpHeaders.Accept, "application/vnd.github+json")
        }
        if (!resp.status.isSuccess()) {
            throw IOException("auth: installation token exchange failed ${resp.status.value}: ${resp.bodyAsText().take(256)}")
        }
        val dto = authJson.decodeFromString<AccessTokenDto>(resp.bodyAsText())
        if (dto.token.isEmpty()) throw IOException("auth: installation token exchange returned an empty token")
        // Fall back to a conservative ~1h lifetime if the timestamp is missing/unparseable, so a
        // malformed response still refreshes rather than caching a token forever.
        val expiresAt = runCatching { Instant.parse(dto.expiresAt) }.getOrElse { nowAt.plusSeconds(3600) }
        return Cached(token = dto.token, expiresAt = expiresAt)
    }

    private data class Cached(val token: String, val expiresAt: Instant)

    companion object {
        const val DEFAULT_BASE_URL = "https://api.github.com"

        // Refresh this many seconds before the stated expiry so a token in flight never goes stale
        // mid-request (GitHub installation tokens last ~1h).
        private const val REFRESH_SKEW_SECONDS = 60L
        private const val EXCHANGE_TIMEOUT_MS = 30_000L
        private const val CONNECT_TIMEOUT_MS = 10_000L
    }
}

/**
 * Builds an App provider pinned to one installation. [privateKeyPem] is the App private key in PEM
 * form (PKCS#1 or PKCS#8); it is parsed and validated as RSA here, throwing
 * [IllegalArgumentException] on an invalid or non-RSA key. [baseUrl] / [httpClient] / [now] are
 * injectable for tests; production uses the defaults.
 */
fun newAppProvider(
    appId: Long,
    installationId: Long,
    privateKeyPem: String,
    baseUrl: String = AppProvider.DEFAULT_BASE_URL,
    httpClient: HttpClient? = null,
    now: () -> Instant = Instant::now,
): AppProvider = AppProvider(appId, installationId, parseRsaPrivateKey(privateKeyPem), baseUrl, httpClient, now)

/**
 * Parses [pem] into an RSA private key, accepting both PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`,
 * the shape GitHub hands out) and PKCS#8 (`-----BEGIN PRIVATE KEY-----`) via Bouncy Castle. Throws
 * [IllegalArgumentException] if the PEM does not parse or is not an RSA key — the JVM analogue of the
 * Go reference's x509 PKCS#1/PKCS#8 validation, failing fast at startup rather than cryptically at
 * the first token exchange.
 */
fun parseRsaPrivateKey(pem: String): RSAPrivateKey {
    val obj = try {
        PEMParser(StringReader(pem)).use { it.readObject() }
    } catch (e: IOException) {
        throw IllegalArgumentException("GitHub App private key is not valid PEM: ${e.message}", e)
    } ?: throw IllegalArgumentException("GitHub App private key is not valid PEM (no PEM block found)")

    val converter = JcaPEMKeyConverter()
    val key: PrivateKey = when (obj) {
        is PEMKeyPair -> converter.getKeyPair(obj).private // PKCS#1 (SEC1 for EC) — an embedded key pair
        is PrivateKeyInfo -> converter.getPrivateKey(obj) // PKCS#8 — a bare private key
        else -> throw IllegalArgumentException("GitHub App private key does not parse as a private key")
    }
    return key as? RSAPrivateKey
        ?: throw IllegalArgumentException("GitHub App private key must be RSA, got ${key.algorithm}")
}

/**
 * Signs an RS256 App JWT for [appId] valid for a short window around [now]. The issued-at is
 * backdated 60s to tolerate clock skew and the expiry is 9 minutes out (under GitHub's 10-minute
 * cap). Used only to authenticate the installation-token exchange.
 */
internal fun buildAppJwt(appId: Long, key: RSAPrivateKey, now: Instant): String {
    val enc = Base64.getUrlEncoder().withoutPadding()
    val header = enc.encodeToString("""{"alg":"RS256","typ":"JWT"}""".toByteArray(Charsets.UTF_8))
    val iat = now.epochSecond - 60
    val exp = now.epochSecond + 540
    val payload = enc.encodeToString("""{"iat":$iat,"exp":$exp,"iss":$appId}""".toByteArray(Charsets.UTF_8))
    val signingInput = "$header.$payload"
    val signer = Signature.getInstance("SHA256withRSA")
    signer.initSign(key)
    signer.update(signingInput.toByteArray(Charsets.UTF_8))
    val signature = enc.encodeToString(signer.sign())
    return "$signingInput.$signature"
}

private val authJson = Json { ignoreUnknownKeys = true }

@Serializable
private data class AccessTokenDto(
    val token: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
)
