package cloud.terence.clowder

import android.net.LocalSocket
import android.net.LocalSocketAddress
import org.json.JSONObject
import java.io.File

/**
 * The clow CLI's IPC protocol: one JSON request, one JSON response,
 * over the daemon's unix socket (daemon/ipc.go, CallIPC / serveIPCConn).
 * The app is a second CLI: same wire, same ops.
 */

data class Cat(
    val name: String,
    val storer: Boolean,
    val dropbox: Boolean,
    val key: String,
)

data class OutboxEntry(
    val id: String,
    val targetName: String,
    val fileName: String,
)

data class Transfer(
    val fileName: String,
    val peer: String,
    val receiving: Boolean,
    val done: Long,
    val total: Long,
)

data class Status(
    val ok: Boolean,
    val error: String,
    val message: String,
    val me: Cat?,
    val cats: List<Cat>,
    val liveness: Map<String, Long>,
    val outbox: List<OutboxEntry>,
    val transfers: List<Transfer>,
    val sentFiles: Long,
    val sentBytes: Long,
    val receivedFiles: Long,
    val receivedBytes: Long,
)

/** Builds the request JSON for an op, mirroring daemon.Request's fields. */
private fun requestJson(op: String, target: String? = null, path: String? = null, words: String? = null): String {
    val o = JSONObject()
    o.put("op", op)
    target?.let { o.put("target", it) }
    path?.let { o.put("path", it) }
    words?.let { o.put("words", it) }
    return o.toString()
}

/** One IPC round trip. Long ops (join, leave) stream step logs — one
 *  JSON object per line, each carrying a "log" field — before the final
 *  response, so return the last line without one. Throws on socket
 *  errors; protocol refusals come back as ok=false with the reason. */
fun ipc(socket: File, op: String, target: String? = null, path: String? = null, words: String? = null): JSONObject {
    val s = LocalSocket()
    try {
        s.connect(LocalSocketAddress(socket.absolutePath, LocalSocketAddress.Namespace.FILESYSTEM))
        s.outputStream.write((requestJson(op, target, path, words) + "\n").toByteArray())
        s.outputStream.flush()
        s.shutdownOutput()
        var final: JSONObject? = null
        for (line in s.inputStream.readBytes().toString(Charsets.UTF_8).lineSequence()) {
            if (line.isBlank()) continue
            val o = JSONObject(line)
            if (o.optString("log", "").isEmpty()) final = o
        }
        return final ?: throw IllegalStateException("daemon sent no response")
    } finally {
        runCatching { s.close() }
    }
}

/** ipc() for user-triggered ops: the daemon's socket appears a second
 *  or two after the app opens, so a send fired in that window races
 *  connect with ENOENT — wait out the startup instead of failing. */
fun ipcWait(socket: File, op: String, target: String? = null, path: String? = null, words: String? = null): JSONObject {
    val deadline = System.currentTimeMillis() + 4000
    while (true) {
        try {
            return ipc(socket, op, target, path, words)
        } catch (e: Exception) {
            if (System.currentTimeMillis() > deadline || !ClowdService.running) throw e
            Thread.sleep(250)
        }
    }
}

/** Parses the status op's response into the models the UI renders. */
fun parseStatus(r: JSONObject): Status {
    fun catOf(o: JSONObject) = Cat(
        name = o.optString("name"),
        storer = o.optBoolean("storer", false),
        dropbox = o.optBoolean("dropbox", false),
        key = o.optString("key"),
    )

    val liveness = HashMap<String, Long>()
    r.optJSONObject("liveness")?.let { l ->
        l.keys().forEach { k -> liveness[k] = l.optLong(k) }
    }
    val outbox = ArrayList<OutboxEntry>()
    r.optJSONArray("outbox")?.let { arr ->
        for (i in 0 until arr.length()) {
            arr.getJSONObject(i).let { e ->
                outbox.add(OutboxEntry(e.optString("id"), e.optString("target_name"), e.optString("file_name")))
            }
        }
    }
    val transfers = ArrayList<Transfer>()
    r.optJSONArray("progress")?.let { arr ->
        for (i in 0 until arr.length()) {
            arr.getJSONObject(i).let { p ->
                transfers.add(
                    Transfer(
                        p.optString("file_name"),
                        p.optString("peer"),
                        p.optBoolean("receiving", false),
                        p.optLong("done"),
                        p.optLong("total"),
                    )
                )
            }
        }
    }
    val stats = r.optJSONObject("stats")
    return Status(
        ok = r.optBoolean("ok", false),
        error = r.optString("error", ""),
        message = r.optString("message", ""),
        me = r.optJSONObject("me")?.let { catOf(it) },
        cats = buildList {
            r.optJSONArray("cats")?.let { arr ->
                for (i in 0 until arr.length()) add(catOf(arr.getJSONObject(i)))
            }
        },
        liveness = liveness,
        outbox = outbox,
        transfers = transfers,
        sentFiles = stats?.optLong("sent", 0L) ?: 0L,
        sentBytes = stats?.optLong("sent_bytes", 0L) ?: 0L,
        receivedFiles = stats?.optLong("received", 0L) ?: 0L,
        receivedBytes = stats?.optLong("received_bytes", 0L) ?: 0L,
    )
}

/** Liveness display, mirroring the CLI's: online inside two minutes,
 *  else how long ago (or never). */
fun livenessText(seen: Long?): String {
    if (seen == null || seen == 0L) return "never seen"
    val ago = System.currentTimeMillis() / 1000 - seen
    return if (ago < 120) "online (seen ${ago}s ago)" else "seen ${ago / 60}m ago"
}

fun humanBytes(n: Long): String {
    if (n < 1024) return "$n B"
    var v = n.toDouble()
    var unit = "KiB"
    v /= 1024
    if (v >= 1024) { v /= 1024; unit = "MiB" }
    if (v >= 1024) { v /= 1024; unit = "GiB" }
    return String.format("%.1f %s", v, unit)
}
