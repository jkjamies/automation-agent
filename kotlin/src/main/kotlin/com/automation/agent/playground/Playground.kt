/*
 * A local-only entrypoint that launches an interactive REPL to chat with the configured model, for
 * local development only. It is a separate entrypoint from the service (`app.MainKt`), so it is
 * never part of a deployed artifact, yet still compiled by the normal build so breakage is caught.
 *
 * Run it with `./gradlew playground` (an interactive console chat). To drive the real workflows
 * interactively, swap the chat agent below for a summary / lint-fixer / coverage-fixer agent.
 */
package com.automation.agent.playground

import com.google.adk.kt.agents.Instruction
import com.google.adk.kt.agents.LlmAgent
import com.google.adk.kt.runners.ReplRunner
import com.automation.agent.agent.setup.buildLLM
import com.automation.agent.config.Config
import com.automation.agent.config.OTEL_EXPORTER_CONSOLE
import com.automation.agent.obs.Config as ObsConfig
import com.automation.agent.obs.init as initTracing

fun main() {
    val cfg = Config.load()

    // Local dev defaults to console tracing (spans to stdout) unless an exporter is explicitly set,
    // so a developer sees the span tree without extra config. The set flag comes from config — this
    // entrypoint reads no environment. Flush + release at exit so the last spans are exported.
    val exporter = if (cfg.otelTracesExporterSet) cfg.otelTracesExporter else OTEL_EXPORTER_CONSOLE
    val shutdownTracing = initTracing(
        ObsConfig(
            exporter = exporter,
            serviceName = cfg.otelServiceName,
            otlpEndpoint = cfg.otelExporterOtlpEndpoint,
            otlpHeaders = cfg.otelExporterOtlpHeaders,
            sampler = cfg.otelTracesSampler,
        ),
    )
    Runtime.getRuntime().addShutdownHook(Thread { runCatching { shutdownTracing() } })

    val llm = buildLLM(cfg)

    // A simple chat agent over the configured model.
    val chat =
        LlmAgent(
            name = "automation_agent_playground",
            description = "Local playground for poking the configured model.",
            model = llm,
            instruction =
                Instruction(
                    "You are the automation-agent local playground, backed by the configured model. " +
                        "Help the developer test prompts. Be concise.",
                ),
        )

    ReplRunner(chat).start()
}
