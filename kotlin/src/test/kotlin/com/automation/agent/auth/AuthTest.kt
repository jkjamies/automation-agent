package com.automation.agent.auth

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldStartWith
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.bouncycastle.asn1.pkcs.PrivateKeyInfo
import org.bouncycastle.openssl.jcajce.JcaPEMWriter
import org.bouncycastle.openssl.jcajce.JcaPKCS8Generator
import org.bouncycastle.util.io.pem.PemObject
import java.io.StringWriter
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.time.Instant
import java.time.format.DateTimeFormatter
import java.util.Base64

/** A request the token-exchange stub saw. */
private data class Seen(val method: String, val path: String, val auth: String)

private fun rsaKeyPair(): KeyPair =
    KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()

/** A PKCS#8 (`-----BEGIN PRIVATE KEY-----`) PEM — the JDK's native encoding. */
private fun pkcs8Pem(kp: KeyPair): String {
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(JcaPKCS8Generator(kp.private, null)) }
    return sw.toString()
}

/** A PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`) PEM — the shape GitHub hands out for an App key. */
private fun pkcs1Pem(kp: KeyPair): String {
    val pkcs1 = PrivateKeyInfo.getInstance(kp.private.encoded).parsePrivateKey().toASN1Primitive().encoded
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(PemObject("RSA PRIVATE KEY", pkcs1)) }
    return sw.toString()
}

/** A non-RSA (EC) key in PKCS#8 PEM, to assert the RSA-only check. */
private fun ecPem(): String {
    val kp = KeyPairGenerator.getInstance("EC").apply { initialize(256) }.generateKeyPair()
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(JcaPKCS8Generator(kp.private, null)) }
    return sw.toString()
}

/** Decodes a base64url JWT segment into a JSON object. */
private fun jwtPart(part: String): kotlinx.serialization.json.JsonObject =
    Json.parseToJsonElement(String(Base64.getUrlDecoder().decode(part))).jsonObject

/**
 * A token-exchange stub. Records every request and returns a 201 with [tokenFor] / [expiresFor]
 * computed at response time (so a refresh test can vary them).
 */
private fun stubClient(
    seen: MutableList<Seen>,
    tokenFor: (Int) -> String,
    expiresFor: (Int) -> String,
): HttpClient {
    val engine = MockEngine { request ->
        seen += Seen(
            method = request.method.value,
            path = request.url.encodedPath,
            auth = request.headers[HttpHeaders.Authorization] ?: "",
        )
        val n = seen.size
        respond(
            content = """{"token":"${tokenFor(n)}","expires_at":"${expiresFor(n)}"}""",
            status = HttpStatusCode.Created,
            headers = headersOf(HttpHeaders.ContentType, "application/json"),
        )
    }
    return HttpClient(engine)
}

private const val FAR_FUTURE = "2099-01-01T00:00:00Z"

class AuthTest : BehaviorSpec({
    Given("a static provider") {
        When("asked for a token") {
            Then("it returns the constant for every repo, and empty stays empty") {
                StaticProvider("pat-123").token("owner/repo") shouldBe "pat-123"
                StaticProvider("pat-123").token("x/y") shouldBe "pat-123"
                StaticProvider("").token("a/b") shouldBe "" // empty = anonymous
                StaticProvider().token("a/b") shouldBe ""
            }
        }
    }

    Given("an App provider over a token-exchange stub") {
        When("it mints an installation token") {
            Then("it exchanges an RS256 JWT issued by the App ID at the pinned installation") {
                val seen = mutableListOf<Seen>()
                val provider = newAppProvider(
                    appId = 42L,
                    installationId = 99L,
                    privateKeyPem = pkcs1Pem(rsaKeyPair()),
                    baseUrl = "https://api.github.test",
                    httpClient = stubClient(seen, { "ghs_installation_token" }, { FAR_FUTURE }),
                )

                provider.token("acme/api") shouldBe "ghs_installation_token"

                seen shouldHaveSize 1
                seen[0].method shouldBe "POST"
                // The exchange targets the pinned installation id (no dynamic resolution).
                seen[0].path shouldBe "/app/installations/99/access_tokens"
                seen[0].auth shouldStartWith "Bearer "

                // The exchange authenticates as the App via an RS256 JWT issued by the App ID.
                val parts = seen[0].auth.removePrefix("Bearer ").split(".")
                parts shouldHaveSize 3
                jwtPart(parts[0])["alg"]?.jsonPrimitive?.content shouldBe "RS256"
                jwtPart(parts[1])["iss"]?.jsonPrimitive?.content shouldBe "42"
            }
        }

        When("the cached token is still valid") {
            Then("a second call reuses it without a new exchange") {
                val seen = mutableListOf<Seen>()
                val provider = newAppProvider(
                    42L, 99L, pkcs8Pem(rsaKeyPair()),
                    baseUrl = "https://api.github.test",
                    httpClient = stubClient(seen, { "ghs_installation_token" }, { FAR_FUTURE }),
                )
                provider.token("acme/api")
                provider.token("acme/api")
                seen shouldHaveSize 1 // cached: minted once
            }
        }

        When("time advances past the cached token's expiry") {
            Then("the next call re-mints a fresh token") {
                val seen = mutableListOf<Seen>()
                var current = Instant.parse("2026-06-27T00:00:00Z")
                val provider = newAppProvider(
                    1L, 2L, pkcs1Pem(rsaKeyPair()),
                    baseUrl = "https://api.github.test",
                    // Each exchange's token expires one hour after the mint time (the test clock).
                    httpClient = stubClient(
                        seen,
                        { n -> "tok-$n" },
                        { DateTimeFormatter.ISO_INSTANT.format(current.plusSeconds(3600)) },
                    ),
                    now = { current },
                )

                provider.token("a/b") shouldBe "tok-1"
                provider.token("a/b") shouldBe "tok-1" // within validity → cached
                seen shouldHaveSize 1

                current = current.plusSeconds(3600) // jump past expiry (minus the refresh skew)
                provider.token("a/b") shouldBe "tok-2" // refreshed
                seen shouldHaveSize 2
            }
        }
    }

    Given("private keys in each accepted form") {
        When("parsing them") {
            Then("PKCS#1 and PKCS#8 RSA keys both parse") {
                parseRsaPrivateKey(pkcs1Pem(rsaKeyPair())).algorithm shouldBe "RSA"
                parseRsaPrivateKey(pkcs8Pem(rsaKeyPair())).algorithm shouldBe "RSA"
            }
            Then("a non-RSA key is rejected") {
                val err = shouldThrow<IllegalArgumentException> { parseRsaPrivateKey(ecPem()) }
                err.message shouldContain "RSA"
            }
            Then("garbage that is not PEM is rejected") {
                shouldThrow<IllegalArgumentException> { parseRsaPrivateKey("not a pem key") }
            }
        }
    }

    Given("an invalid private key") {
        When("building an App provider") {
            Then("construction fails fast") {
                shouldThrow<IllegalArgumentException> { newAppProvider(1L, 2L, "not a pem key") }
            }
        }
    }
})
