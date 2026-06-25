package com.automation.agent.webhook

import java.security.MessageDigest
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

/** Checks a GitHub "sha256=<hex>" HMAC over the request body. */
internal fun verifySignature(secret: String, header: String, body: ByteArray): Boolean {
    val prefix = "sha256="
    if (!header.startsWith(prefix)) return false
    val mac = Mac.getInstance("HmacSHA256")
    mac.init(SecretKeySpec(secret.toByteArray(), "HmacSHA256"))
    val want = mac.doFinal(body).toHex()
    val got = header.removePrefix(prefix)
    // Constant-time comparison.
    return MessageDigest.isEqual(want.toByteArray(), got.toByteArray())
}

private fun ByteArray.toHex(): String {
    val hex = CharArray(size * 2)
    val digits = "0123456789abcdef"
    for (i in indices) {
        val v = this[i].toInt() and 0xFF
        hex[i * 2] = digits[v ushr 4]
        hex[i * 2 + 1] = digits[v and 0x0F]
    }
    return String(hex)
}
