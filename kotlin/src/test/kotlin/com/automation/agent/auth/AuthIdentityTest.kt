package com.automation.agent.auth

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respond
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.headersOf
import org.bouncycastle.openssl.jcajce.JcaPEMWriter
import org.bouncycastle.openssl.jcajce.JcaPKCS8Generator
import java.io.StringWriter
import java.security.KeyPairGenerator

/** A stub returning [json] for any request under [path], else 404. */
private fun stub(path: String, json: String): HttpClient {
    val engine = MockEngine { request ->
        if (request.url.encodedPath == path) {
            respond(json, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
        } else {
            respond("", HttpStatusCode.NotFound)
        }
    }
    return HttpClient(engine)
}

private fun pkcs8Pem(): String {
    val kp = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(JcaPKCS8Generator(kp.private, null)) }
    return sw.toString()
}

class AuthIdentityTest : BehaviorSpec({
    Given("StaticProvider.authoredLogin") {
        Then("a non-empty token resolves the authenticated user login via GET /user") {
            val p = StaticProvider(token = "t", baseUrl = "https://api.github.test", httpClient = stub("/user", "{\"login\":\"octocat\"}"))
            p.authoredLogin() shouldBe "octocat"
        }
        Then("an empty token yields \"\" (anonymous, no lookup)") {
            StaticProvider(token = "").authoredLogin() shouldBe ""
        }
    }

    Given("AppProvider.authoredLogin") {
        Then("it resolves \"<slug>[bot]\" via GET /app and caches it") {
            val provider = newAppProvider(
                appId = 1,
                installationId = 2,
                privateKeyPem = pkcs8Pem(),
                baseUrl = "https://api.github.test",
                httpClient = stub("/app", "{\"slug\":\"my-app\"}"),
            )
            provider.authoredLogin() shouldBe "my-app[bot]"
            provider.authoredLogin() shouldBe "my-app[bot]" // served from cache
        }
    }
})
