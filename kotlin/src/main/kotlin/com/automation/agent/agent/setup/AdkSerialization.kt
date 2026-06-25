/*
 * Shared serialization bridge for the hand-rolled durable session services.
 *
 * An ADK `Event` is `@Serializable`, but its `Any`-typed payloads use contextual serializers that
 * are registered only on the SDK's own `Json` instance (`adkJson`), which adk-kotlin marks
 * `internal` (JVM-public). A stock `Json` therefore cannot round-trip an Event. We reach that
 * configured instance reflectively so both the sqlite and firestore session services serialize
 * events exactly as the SDK does. Pinned to adk-kotlin 0.4.0; if the SDK relocates `getAdkJson`
 * this fails fast at startup, which is the right signal.
 */
package com.automation.agent.agent.setup

import kotlinx.serialization.json.Json

internal val adkEventJson: Json = run {
    val method = Class.forName("com.google.adk.kt.serialization.SerializersKt").getMethod("getAdkJson")
    method.invoke(null) as Json
}
