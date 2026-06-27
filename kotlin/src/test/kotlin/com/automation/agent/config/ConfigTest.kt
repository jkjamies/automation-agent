package com.automation.agent.config

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import io.kotest.matchers.string.shouldContain
import io.kotest.matchers.string.shouldNotContain
import org.bouncycastle.asn1.pkcs.PrivateKeyInfo
import org.bouncycastle.openssl.jcajce.JcaPEMWriter
import org.bouncycastle.openssl.jcajce.JcaPKCS8Generator
import org.bouncycastle.util.io.pem.PemObject
import java.io.StringWriter
import java.nio.file.Files
import java.security.KeyPair
import java.security.KeyPairGenerator

private fun lookupOf(m: Map<String, String>): Config.Companion.Lookup =
    Config.Companion.Lookup { m[it] }

private fun rsaPair(): KeyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()

private fun pkcs8Pem(kp: KeyPair): String {
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(JcaPKCS8Generator(kp.private, null)) }
    return sw.toString()
}

private fun pkcs1Pem(kp: KeyPair): String {
    val pkcs1 = PrivateKeyInfo.getInstance(kp.private.encoded).parsePrivateKey().toASN1Primitive().encoded
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(PemObject("RSA PRIVATE KEY", pkcs1)) }
    return sw.toString()
}

private fun ecPem(): String {
    val kp = KeyPairGenerator.getInstance("EC").apply { initialize(256) }.generateKeyPair()
    val sw = StringWriter()
    JcaPEMWriter(sw).use { it.writeObject(JcaPKCS8Generator(kp.private, null)) }
    return sw.toString()
}

/** A throwaway RSA key in PKCS#8 PEM, reused across the App-mode cases. */
private val APP_PEM: String = pkcs8Pem(rsaPair())

/** The full App env, with [overrides] merged in. An override value of "" reads as unset (getOr). */
private fun appEnv(overrides: Map<String, String> = emptyMap()): Map<String, String> {
    val base = mutableMapOf(
        "GITHUB_APP_ID" to "42",
        "GITHUB_APP_INSTALLATION_ID" to "99",
        "GITHUB_APP_PRIVATE_KEY" to APP_PEM,
        "REPOS" to "acme/api",
    )
    base.putAll(overrides)
    return base
}

private fun writeKeyFile(contents: String): String {
    val p = Files.createTempFile("config-app-", ".pem")
    Files.writeString(p, contents)
    return p.toString()
}

class ConfigTest : BehaviorSpec({
    Given("an environment with no variables set") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(emptyMap()))
            Then("it applies the documented defaults") {
                c.llmProvider shouldBe Provider.OLLAMA
                c.ollamaModel shouldBe "gemma4:12b"
                c.ollamaCodeModel shouldBe "gemma4:26b"
                c.notifyProvider shouldBe NotifyProvider.SLACK
                c.maxIterations shouldBe 3
                c.ciTimeout.inWholeMinutes shouldBe 90L
                c.agentPrLabel shouldBe "automation-agent"
                c.sessionBackend shouldBe SessionBackend.MEMORY
                c.sqliteDsn shouldBe "automation-agent.db"
                c.firestoreProject shouldBe ""
                c.firestoreCollection shouldBe "automation_agent"
                c.internalToken shouldBe ""
                c.gitTransport shouldBe "https"
                c.gitSshKey shouldBe ""
            }
        }
    }

    Given("an ssh git transport with an explicit key") {
        When("loading the configuration") {
            val c = Config.loadFrom(
                lookupOf(
                    mapOf(
                        "GIT_TRANSPORT" to "ssh",
                        "GIT_SSH_KEY" to "/home/dev/.ssh/id_ed25519",
                    ),
                ),
            )
            Then("the transport and key path are read") {
                c.gitTransport shouldBe "ssh"
                c.gitSshKey shouldBe "/home/dev/.ssh/id_ed25519"
            }
        }
    }

    Given("an invalid GIT_TRANSPORT") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("GIT_TRANSPORT" to "scp")))
                }
            }
        }
    }

    Given("explicit session-backend settings") {
        When("loading the sqlite backend with overrides") {
            val c = Config.loadFrom(
                lookupOf(
                    mapOf(
                        "SESSION_BACKEND" to "sqlite",
                        "SQLITE_DSN" to "/data/agent.db",
                        "INTERNAL_TOKEN" to "sekret",
                    ),
                ),
            )
            Then("the backend, DSN, and internal token are read") {
                c.sessionBackend shouldBe SessionBackend.SQLITE
                c.sqliteDsn shouldBe "/data/agent.db"
                c.internalToken shouldBe "sekret"
            }
        }

        When("loading the firestore backend with overrides") {
            val c = Config.loadFrom(
                lookupOf(
                    mapOf(
                        "SESSION_BACKEND" to "firestore",
                        "FIRESTORE_PROJECT" to "my-proj",
                        "FIRESTORE_COLLECTION" to "agent_runs",
                    ),
                ),
            )
            Then("the backend, project, and collection are read") {
                c.sessionBackend shouldBe SessionBackend.FIRESTORE
                c.firestoreProject shouldBe "my-proj"
                c.firestoreCollection shouldBe "agent_runs"
            }
        }
    }

    Given("an invalid SESSION_BACKEND") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("SESSION_BACKEND" to "postgres")))
                }
            }
        }
    }

    Given("a REPOS value with surrounding whitespace and empty entries") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(mapOf("REPOS" to " a/b , c/d ,, e/f ")))
            Then("repositories are trimmed and empties dropped") {
                c.repos shouldBe listOf("a/b", "c/d", "e/f")
            }
        }
    }

    Given("an explicit OLLAMA_CODE_MODEL override") {
        When("loading the configuration") {
            val c = Config.loadFrom(
                lookupOf(mapOf("OLLAMA_MODEL" to "gemma4:12b", "OLLAMA_CODE_MODEL" to "gemma4:26b")),
            )
            Then("the code model is used and the base model is unchanged") {
                c.ollamaCodeModel shouldBe "gemma4:26b"
                c.ollamaModel shouldBe "gemma4:12b"
            }
        }
    }

    Given("GH_TOKEN set but GITHUB_TOKEN unset") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(mapOf("GH_TOKEN" to "gh_abc")))
            Then("GH_TOKEN is honoured so a local gh-style env works") {
                c.githubToken shouldBe "gh_abc"
            }
        }
    }

    Given("both GITHUB_TOKEN and GH_TOKEN set") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(mapOf("GITHUB_TOKEN" to "primary", "GH_TOKEN" to "fallback")))
            Then("GITHUB_TOKEN takes precedence") {
                c.githubToken shouldBe "primary"
            }
        }
    }

    Given("a compound CI_TIMEOUT duration") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(mapOf("CI_TIMEOUT" to "1h30m")))
            Then("it parses to the summed duration") {
                c.ciTimeout.inWholeMinutes shouldBe 90L
            }
        }
    }

    Given("an invalid LLM_PROVIDER") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("LLM_PROVIDER" to "openai")))
                }
            }
        }
    }

    Given("an invalid NOTIFY_PROVIDER") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("NOTIFY_PROVIDER" to "discord")))
                }
            }
        }
    }

    Given("an unparseable CI_TIMEOUT") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("CI_TIMEOUT" to "soon")))
                }
            }
        }
    }

    Given("a non-numeric MAX_ITERATIONS") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("MAX_ITERATIONS" to "lots")))
                }
            }
        }
    }

    Given("MAX_ITERATIONS below the floor") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("MAX_ITERATIONS" to "0")))
                }
            }
        }
    }

    Given("a non-numeric PORT") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("PORT" to "abc")))
                }
            }
        }
    }

    Given("a PORT out of range") {
        When("loading the configuration") {
            Then("it fails") {
                shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(mapOf("PORT" to "70000")))
                }
            }
        }
    }

    Given("no GitHub App vars") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(mapOf("GITHUB_TOKEN" to "pat", "REPOS" to "acme/api")))
            Then("it stays in PAT mode with zero App fields") {
                c.appMode() shouldBe false
                c.githubAppId shouldBe 0L
                c.githubAppInstallationId shouldBe 0L
                c.githubAppPrivateKeyPem shouldBe ""
            }
        }
    }

    Given("the full GitHub App var set") {
        When("loading the configuration") {
            val c = Config.loadFrom(lookupOf(appEnv()))
            Then("App mode is selected and the credentials are read") {
                c.appMode() shouldBe true
                c.githubAppId shouldBe 42L
                c.githubAppInstallationId shouldBe 99L
                c.githubAppPrivateKeyPem shouldContain "-----BEGIN"
            }
        }

        When("the key is a PKCS#1 RSA key") {
            val c = Config.loadFrom(lookupOf(appEnv(mapOf("GITHUB_APP_PRIVATE_KEY" to pkcs1Pem(rsaPair())))))
            Then("it is accepted") {
                c.appMode() shouldBe true
            }
        }

        When("the key is read from a file") {
            val path = writeKeyFile(APP_PEM)
            val c = Config.loadFrom(
                lookupOf(appEnv(mapOf("GITHUB_APP_PRIVATE_KEY" to "", "GITHUB_APP_PRIVATE_KEY_PATH" to path))),
            )
            Then("App mode is selected from the file contents") {
                c.appMode() shouldBe true
                c.githubAppPrivateKeyPem shouldContain "-----BEGIN"
            }
        }

        When("the key is flattened to literal \\n") {
            val c = Config.loadFrom(lookupOf(appEnv(mapOf("GITHUB_APP_PRIVATE_KEY" to APP_PEM.replace("\n", "\\n")))))
            Then("the escaped newlines are restored") {
                c.appMode() shouldBe true
                c.githubAppPrivateKeyPem shouldNotContain "\\n"
            }
        }

        When("the key is flattened AND has a real trailing newline (from a file)") {
            // A secret store can flatten newlines to literal `\n` and still append one real trailing
            // newline; the unescape must run on the escaped sequences regardless. The file path is
            // read untrimmed, exercising the corrected condition directly.
            val path = writeKeyFile(APP_PEM.replace("\n", "\\n") + "\n")
            val c = Config.loadFrom(
                lookupOf(appEnv(mapOf("GITHUB_APP_PRIVATE_KEY" to "", "GITHUB_APP_PRIVATE_KEY_PATH" to path))),
            )
            Then("the escaped newlines are still restored") {
                c.appMode() shouldBe true
                c.githubAppPrivateKeyPem shouldNotContain "\\n"
            }
        }
    }

    Given("a config holding secrets") {
        When("it is stringified (e.g. by a debug/startup log)") {
            val c = Config.loadFrom(
                lookupOf(
                    appEnv(
                        mapOf(
                            "GITHUB_TOKEN" to "ghp_supersecretpat",
                            "GITHUB_WEBHOOK_SECRET" to "webhook-shhh",
                            "INTERNAL_TOKEN" to "internal-shhh",
                            "SLACK_WEBHOOK_URL" to "https://hooks.slack.com/services/SECRETPATH",
                        ),
                    ),
                ),
            )
            val s = c.toString()
            Then("every credential value is masked and none leaks verbatim") {
                s shouldNotContain "ghp_supersecretpat"
                s shouldNotContain "webhook-shhh"
                s shouldNotContain "internal-shhh"
                s shouldNotContain "SECRETPATH"
                s shouldNotContain "-----BEGIN" // the App private key PEM must never appear
                s shouldContain "***"
                s shouldContain "githubAppId=42" // non-secret fields stay visible for debugging
            }
        }
    }

    Given("a misconfigured GitHub App env") {
        listOf(
            "missing app id" to mapOf("GITHUB_APP_ID" to ""),
            "missing installation" to mapOf("GITHUB_APP_INSTALLATION_ID" to ""),
            "missing key" to mapOf("GITHUB_APP_PRIVATE_KEY" to ""),
            "both key sources" to mapOf("GITHUB_APP_PRIVATE_KEY_PATH" to "/some/key.pem"),
            "zero app id" to mapOf("GITHUB_APP_ID" to "0"),
            "negative app id" to mapOf("GITHUB_APP_ID" to "-1"),
            "non-numeric app id" to mapOf("GITHUB_APP_ID" to "abc"),
            "zero installation" to mapOf("GITHUB_APP_INSTALLATION_ID" to "0"),
            "non-numeric installation" to mapOf("GITHUB_APP_INSTALLATION_ID" to "x"),
            "invalid pem" to mapOf("GITHUB_APP_PRIVATE_KEY" to "not a pem"),
            "empty repos in app mode" to mapOf("REPOS" to ""),
        ).forEach { (name, overrides) ->
            When("loading with $name") {
                Then("it fails") {
                    shouldThrow<IllegalArgumentException> { Config.loadFrom(lookupOf(appEnv(overrides))) }
                }
            }
        }

        When("the key is a non-RSA (EC) key") {
            Then("it is rejected as not RSA") {
                val err = shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(lookupOf(appEnv(mapOf("GITHUB_APP_PRIVATE_KEY" to ecPem()))))
                }
                err.message shouldContain "RSA"
            }
        }

        When("the key file is unreadable") {
            Then("it reports the read failure") {
                // A guaranteed-missing child of a fresh temp dir, so the read-failure branch is
                // exercised deterministically (a host-dependent literal path could exist on a runner).
                val missing = Files.createTempDirectory("config-app-missing-").resolve("missing.pem").toString()
                val err = shouldThrow<IllegalArgumentException> {
                    Config.loadFrom(
                        lookupOf(
                            appEnv(
                                mapOf(
                                    "GITHUB_APP_PRIVATE_KEY" to "",
                                    "GITHUB_APP_PRIVATE_KEY_PATH" to missing,
                                ),
                            ),
                        ),
                    )
                }
                err.message shouldContain "read GITHUB_APP_PRIVATE_KEY_PATH"
            }
        }
    }
})
