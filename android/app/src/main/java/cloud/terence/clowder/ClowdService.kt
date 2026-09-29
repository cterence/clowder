package cloud.terence.clowder

import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.app.Service
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.provider.MediaStore
import android.util.Log
import android.webkit.MimeTypeMap
import android.widget.Toast
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.util.concurrent.atomic.AtomicBoolean

/**
 * Runs the clowder daemon as a child process while the app is open:
 * MainActivity starts it in onStart and stops it in onStop, so the
 * daemon uses no battery in the background (no foreground service, no
 * wake lock). The daemon binary ships as libclowder.so in jniLibs
 * (useLegacyPackaging extracts it to nativeLibraryDir, which is
 * executable) and is a plain GOOS=android build: tailcat's
 * android_linux.go patches DNS, CA certs and interface discovery at
 * init.
 *
 * The app talks to the daemon over the same unix-socket IPC the clow
 * CLI uses; nothing here touches the daemon package.
 */
class ClowdService : Service() {

    companion object {
        const val ACTION_START = "cloud.terence.clowder.START"
        const val ACTION_STOP = "cloud.terence.clowder.STOP"
        private const val BINARY = "libclowder.so"
        private const val TAG = "clowd"

        @Volatile var running = false
            private set

        /** The daemon's recent stdout, newest last, for the debug view. */
        val logLines = ArrayDeque<String>()

        private fun appendLog(line: String) {
            synchronized(logLines) {
                logLines.addLast(line)
                while (logLines.size > 400) logLines.removeFirst()
            }
        }

        fun recentLog(): String = synchronized(logLines) { logLines.joinToString("\n") }

        fun clearLog() = synchronized(logLines) { logLines.clear() }

        fun configDir(ctx: Context): File = File(ctx.filesDir, "clowder")
        fun socketFile(ctx: Context): File = File(configDir(ctx), "clow.sock")
        fun inboxDir(ctx: Context): File = File(File(ctx.filesDir, "Downloads"), "clowder")
        fun binaryFile(ctx: Context): File =
            File(ctx.applicationInfo.nativeLibraryDir, BINARY)

        // The inbox is a receipt log, not a directory view: files can be
        // deleted from Downloads without losing the record.
        private val inboxLogLock = Any()
        fun inboxLogFile(ctx: Context): File = File(ctx.filesDir, "inbox-log.json")

        fun appendInboxLog(ctx: Context, name: String, bytes: Long) {
            synchronized(inboxLogLock) {
                val arr = runCatching { JSONArray(inboxLogFile(ctx).readText()) }.getOrElse { JSONArray() }
                arr.put(
                    JSONObject()
                        .put("name", name)
                        .put("bytes", bytes)
                        .put("at", System.currentTimeMillis()),
                )
                while (arr.length() > 200) arr.remove(0)
                inboxLogFile(ctx).writeText(arr.toString())
            }
        }

        fun readInboxLog(ctx: Context): JSONArray = synchronized(inboxLogLock) {
            runCatching { JSONArray(inboxLogFile(ctx).readText()) }.getOrElse { JSONArray() }
        }

        fun clearInboxLog(ctx: Context) = synchronized(inboxLogLock) {
            inboxLogFile(ctx).writeText("[]")
        }

        fun isInitialized(ctx: Context): Boolean =
            File(configDir(ctx), "me.json").isFile

        fun start(ctx: Context) {
            ctx.startService(Intent(ctx, ClowdService::class.java).setAction(ACTION_START))
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, ClowdService::class.java).setAction(ACTION_STOP))
        }
    }

    private var daemon: Process? = null
    private val stopping = AtomicBoolean(false)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopping.set(true)
                killDaemon()
                Log.i(TAG, "daemon stopped")
                stopSelf()
            }
            else -> {
                stopping.set(false)
                if (daemon?.isAlive != true) {
                    Thread { runDaemon() }.start()
                }
                if (publisher?.isAlive != true) publisher = Thread { runPublisher() }.also { it.start() }
            }
        }
        return START_NOT_STICKY
    }

    private var publisher: Thread? = null

    /** Moves delivered files from the sandbox inbox into the system's
     *  Downloads (Download/clowder), mirroring the desktop inbox: the
     *  file manager and every other app see them natively. IS_PENDING
     *  keeps each move invisible until the copy completes. */
    private fun runPublisher() {
        while (!stopping.get()) {
            runCatching { publishInbox() }
            var slept = 0L
            while (!stopping.get() && slept < 4000) {
                Thread.sleep(250)
                slept += 250
            }
        }
    }

    private fun publishInbox() {
        // ".tmp-*" is an interrupted atomic write: never promote a
        // partial file into Downloads (the daemon sweeps them too).
        val files = inboxDir(this).listFiles()
            ?.filter { it.isFile && !it.name.startsWith(".tmp-") }
            ?: return
        for (f in files) {
            val size = f.length()
            val mime = MimeTypeMap.getSingleton()
                .getMimeTypeFromExtension(f.extension) ?: "application/octet-stream"
            val values = ContentValues().apply {
                put(MediaStore.MediaColumns.DISPLAY_NAME, f.name)
                put(MediaStore.MediaColumns.MIME_TYPE, mime)
                put(MediaStore.MediaColumns.RELATIVE_PATH, "Download/clowder")
                put(MediaStore.MediaColumns.IS_PENDING, 1)
            }
            val uri = contentResolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
                ?: continue
            val copied = runCatching {
                contentResolver.openOutputStream(uri)?.use { out ->
                    f.inputStream().use { it.copyTo(out) }
                } != null
            }.getOrDefault(false)
            values.clear()
            values.put(MediaStore.MediaColumns.IS_PENDING, 0)
            runCatching { contentResolver.update(uri, values, null, null) }
            if (copied) {
                f.delete()
                appendInboxLog(this, f.name, size)
                appendLog("inbox: ${f.name} moved to Downloads/clowder")
                // Delivery only happens while the app is open, so a
                // toast is visible exactly when it lands.
                Handler(Looper.getMainLooper()).post {
                    Toast.makeText(this, "received ${f.name}", Toast.LENGTH_SHORT).show()
                }
            }
        }
    }

    /** Runs the daemon while the app is open, restarting on unexpected
     *  exits with a simple backoff; a crash loop must not spin the CPU. */
    private fun runDaemon() {
        var restarts = 0
        while (!stopping.get()) {
            val dir = configDir(this)
            if (!isInitialized(this)) {
                appendLog("not initialized: run init first")
                return
            }
            if (!dir.isDirectory) dir.mkdirs()
            running = true
            try {
                val proc = ProcessBuilder(binaryFile(this).absolutePath, "daemon")
                    .redirectErrorStream(true)
                    .apply {
                        environment()["CLOWDER_DIR"] = dir.absolutePath
                        environment()["HOME"] = filesDir.absolutePath
                    }
                    .start()
                daemon = proc
                appendLog("daemon started")
                proc.inputStream.bufferedReader().useLines { lines ->
                    for (line in lines) {
                        Log.i(TAG, line)
                        appendLog(line)
                    }
                }
                val code = proc.waitFor()
                appendLog("daemon exited (code $code)")
                daemon = null
            } catch (e: Exception) {
                appendLog("daemon failed to start: ${e.message}")
                daemon = null
            } finally {
                running = false
            }
            if (stopping.get()) return
            restarts++
            val backoffMs = (1000L shl restarts.coerceAtMost(5)).coerceAtMost(30_000L)
            appendLog("restarting daemon in ${backoffMs / 1000}s (restart #$restarts)")
            var slept = 0L
            while (!stopping.get() && slept < backoffMs) {
                Thread.sleep(250)
                slept += 250
            }
        }
    }

    private fun killDaemon() {
        running = false
        daemon?.apply {
            destroy()
            runCatching { waitFor(5, java.util.concurrent.TimeUnit.SECONDS) ?: destroyForcibly() }
        }
        daemon = null
    }

    override fun onDestroy() {
        stopping.set(true)
        killDaemon()
        super.onDestroy()
    }
}
