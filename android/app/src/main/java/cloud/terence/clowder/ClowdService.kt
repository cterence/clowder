package cloud.terence.clowder

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
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
 * daemon uses no battery in the background (no always-on foreground
 * service, no wake lock). The one exception is stopWhenIdle(): a
 * transfer already in flight when the app leaves the screen runs to
 * completion, the service holding it as a dataSync foreground service
 * until the daemon is idle — then it stops as onStop would have.
 * The daemon binary ships as libclowder.so in jniLibs
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
        const val ACTION_STOP_WHEN_IDLE = "cloud.terence.clowder.STOP_WHEN_IDLE"
        private const val BINARY = "libclowder.so"
        private const val TAG = "clowd"
        private const val NOTIF_ID = 1
        private const val NOTIF_CHANNEL = "transfers"

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

        fun configBase(ctx: Context): File = File(ctx.filesDir, "clowder")
        fun socketFile(ctx: Context): File = File(configDir(ctx), "clow.sock")
        fun inboxDir(ctx: Context): File =
            File(File(File(ctx.filesDir, "Downloads"), "clowder"), activeClowder(ctx))
        fun binaryFile(ctx: Context): File =
            File(ctx.applicationInfo.nativeLibraryDir, BINARY)

        // A clowder is a config dir under the base, named locally —
        // never on the wire. The binary resolves CLOWDER_DIR the same
        // way, so subprocesses get the base plus CLOWDER=<name>.
        private const val PREFS = "clowder"
        private const val PREF_ACTIVE = "active"

        fun activeClowder(ctx: Context): String =
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(PREF_ACTIVE, "default") ?: "default"

        fun setActiveClowder(ctx: Context, name: String) {
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putString(PREF_ACTIVE, name).apply()
        }

        fun validClowderName(name: String): Boolean =
            Regex("^[a-z][a-z0-9-]{0,31}$").matches(name)

        /** The clowder dirs that exist, "default" always offered. */
        fun listClowders(ctx: Context): List<String> =
            ((configBase(ctx).listFiles()?.filter { it.isDirectory }?.map { it.name } ?: emptyList()) + "default")
                .distinct()
                .sorted()

        /** The pre-nesting layout (identity.json directly in the base,
         *  loose files directly in the sandbox inbox) migrates into
         *  default/ once, mirroring the CLI's migration; it runs
         *  before the daemon ever starts. */
        private fun migrateFlat(ctx: Context) {
            val base = configBase(ctx)
            if (File(base, "identity.json").isFile) {
                val def = File(base, "default")
                def.mkdirs()
                base.listFiles()?.forEach { f ->
                    if (f.name != "default" && f.name != "identity.json") {
                        f.renameTo(File(def, f.name))
                    }
                }
                File(base, "identity.json").renameTo(File(def, "identity.json"))
            }
            val inbox = File(File(ctx.filesDir, "Downloads"), "clowder")
            val defInbox = File(inbox, "default")
            val loose = inbox.listFiles()?.filter { it.isFile } ?: emptyList()
            if (loose.isNotEmpty() && !defInbox.isDirectory) {
                defInbox.mkdirs()
                loose.forEach { it.renameTo(File(defInbox, it.name)) }
            }
        }

        fun configDir(ctx: Context): File {
            migrateFlat(ctx)
            return File(configBase(ctx), activeClowder(ctx))
        }

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

        /** stop() for the activity lifecycle: a transfer already in
         *  flight runs to completion in the foreground first, then the
         *  service stops itself. */
        fun stopWhenIdle(ctx: Context) {
            ctx.startService(Intent(ctx, ClowdService::class.java).setAction(ACTION_STOP_WHEN_IDLE))
        }
    }

    private val holding = AtomicBoolean(false)

    private var daemon: Process? = null
    private val stopping = AtomicBoolean(false)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> stopNow()
            ACTION_STOP_WHEN_IDLE -> {
                if (holding.compareAndSet(false, true)) Thread { holdUntilIdle() }.start()
            }
            else -> {
                holding.set(false)
                stopping.set(false)
                if (daemon?.isAlive != true) {
                    Thread { runDaemon() }.start()
                }
                if (publisher?.isAlive != true) publisher = Thread { runPublisher() }.also { it.start() }
            }
        }
        return START_NOT_STICKY
    }

    /** In-flight transfers (either direction) per the status op; a dead
     *  socket counts as none — nothing to hold the service for. */
    private fun transfers(): List<Transfer> = runCatching {
        parseStatus(ipc(socketFile(this), "status")).transfers
    }.getOrDefault(emptyList())

    /** Foreground hold: promote to a dataSync service while a transfer
     *  is in flight, mirror its progress into the notification, and
     *  stop as soon as the daemon goes idle (or ACTION_START cancels
     *  the hold — the app is back on screen). */
    private fun holdUntilIdle() {
        var shown = false
        while (holding.get()) {
            val active = transfers()
            if (active.isEmpty()) break
            val n = transferNotification(active)
            if (shown) {
                getSystemService(NotificationManager::class.java).notify(NOTIF_ID, n)
            } else {
                startForeground(NOTIF_ID, n)
                shown = true
            }
            var slept = 0L
            while (holding.get() && slept < 3000) {
                Thread.sleep(250)
                slept += 250
            }
        }
        if (holding.compareAndSet(true, false)) stopNow()
    }

    private fun transferNotification(active: List<Transfer>): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(NOTIF_CHANNEL, "Transfers", NotificationManager.IMPORTANCE_LOW),
        )
        val t = active.first()
        val dir = if (t.receiving) "Receiving" else "Sending"
        val text = if (active.size == 1) {
            "$dir ${t.fileName} (${humanBytes(t.done)}/${humanBytes(t.total)})"
        } else {
            "$dir ${t.fileName} + ${active.size - 1} more"
        }
        return Notification.Builder(this, NOTIF_CHANNEL)
            .setContentTitle("Clowder")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_download)
            .setProgress(t.total.coerceIn(0, Int.MAX_VALUE.toLong()).toInt(), t.done.coerceIn(0, Int.MAX_VALUE.toLong()).toInt(), false)
            .setOngoing(true)
            .build()
    }

    private fun stopNow() {
        holding.set(false)
        stopping.set(true)
        stopForeground(STOP_FOREGROUND_REMOVE)
        killDaemon()
        Log.i(TAG, "daemon stopped")
        stopSelf()
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
                // MediaStore dedupes names: a second "photo.jpg" saves
                // as "photo (1).jpg". Log the saved name — the inbox
                // shows the copy suffix and still resolves the file.
                val savedName = contentResolver.query(
                    uri, arrayOf(MediaStore.MediaColumns.DISPLAY_NAME), null, null, null,
                )?.use { c -> if (c.moveToFirst()) c.getString(0) else null } ?: f.name
                f.delete()
                appendInboxLog(this, savedName, size)
                appendLog("inbox: ${savedName} moved to Downloads/clowder")
                // Delivery only happens while the app is open (or held
                // open by a transfer), so a toast is visible exactly
                // when it lands.
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
                        // The binary resolves CLOWDER_DIR the same way we do:
                        // the base plus CLOWDER=<name> names this clowder.
                        environment()["CLOWDER_DIR"] = configBase(this@ClowdService).absolutePath
                        environment()["CLOWDER"] = activeClowder(this@ClowdService)
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
