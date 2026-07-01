/*
 * Shared helpers for the obs tests. Deterministic: an in-memory exporter records spans, and the OTel
 * global is reset around each use so a test can register a fresh provider (the global refuses a
 * second registration, and a prior GlobalOpenTelemetry.get() auto-installs a no-op that would block
 * ours — resetForTest clears both).
 */
package com.automation.agent.obs

import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter

/**
 * Install a recording provider over an [InMemorySpanExporter], run [block], then flush + release and
 * reset the OTel global. The exporter uses a batch processor (like production), so a test observes a
 * span only after [flush].
 */
internal fun <T> withRecording(block: (InMemorySpanExporter) -> T): T {
    GlobalOpenTelemetry.resetForTest()
    val exporter = InMemorySpanExporter.create()
    val shutdown = install(exporter, Config(exporter = EXPORTER_CONSOLE, serviceName = "automation-agent"))
    try {
        return block(exporter)
    } finally {
        shutdown()
        GlobalOpenTelemetry.resetForTest()
    }
}
