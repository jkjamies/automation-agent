package io.github.jkjamies.automationagent.config

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe

private fun lookupOf(m: Map<String, String>): Config.Companion.Lookup =
    Config.Companion.Lookup { m[it] }

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
                c.agentCheckName shouldBe "agent-lint-verify"
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
})
