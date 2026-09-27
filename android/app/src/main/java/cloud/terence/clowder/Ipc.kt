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

/** A delivery confirmation for a file we sent (daemon.Receipt). */
data class Receipt(
    val fileName: String,
    val from: String,
    val deliveredAt: Long,
)

data class Status(
    val ok: Boolean,
    val error: String,
    val message: String,
    val me: Cat?,
    val cats: List<Cat>,
    val liveness: Map<String, Long>,
    val paths: Map<String, PathInfo>,
    val outbox: List<OutboxEntry>,
    val transfers: List<Transfer>,
    val receipts: List<Receipt>,
    val sentFiles: Long,
    val sentBytes: Long,
    val receivedFiles: Long,
    val receivedBytes: Long,
)

data class PathInfo(val direct: Boolean, val endpoint: String)

/** Builds the request JSON for an op, mirroring daemon.Request's fields. */
private fun requestJson(op: String, target: String? = null, path: String? = null, words: String? = null): String {
    val o = JSONObject()
    o.put("op", op)
    target?.let { o.put("target", it) }
    path?.let { o.put("path", it) }
    words?.let { o.put("words", it) }
    return o.toString()
}

/** One IPC round trip. Throws on socket errors; protocol refusals come
 *  back as ok=false with the daemon's reason. */
fun ipc(socket: File, op: String, target: String? = null, path: String? = null, words: String? = null): JSONObject {
    val s = LocalSocket()
    try {
        s.connect(LocalSocketAddress(socket.absolutePath, LocalSocketAddress.Namespace.FILESYSTEM))
        s.outputStream.write((requestJson(op, target, path, words) + "\n").toByteArray())
        s.outputStream.flush()
        s.shutdownOutput()
        val bytes = s.inputStream.readBytes()
        return JSONObject(bytes.toString(Charsets.UTF_8))
    } finally {
        runCatching { s.close() }
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
    val paths = HashMap<String, PathInfo>()
    r.optJSONObject("paths")?.let { p ->
        p.keys().forEach { k ->
            p.optJSONObject(k)?.let { info ->
                paths[k] = PathInfo(
                    direct = info.optBoolean("direct", false),
                    endpoint = info.optString("endpoint", ""),
                )
            }
        }
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
    val receipts = ArrayList<Receipt>()
    r.optJSONArray("receipts")?.let { arr ->
        for (i in 0 until arr.length()) {
            arr.getJSONObject(i).let { rc ->
                receipts.add(Receipt(rc.optString("file_name"), rc.optString("from"), rc.optLong("delivered_at")))
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
        paths = paths,
        outbox = outbox,
        transfers = transfers,
        receipts = receipts,
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
