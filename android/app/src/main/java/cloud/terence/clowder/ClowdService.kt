package cloud.terence.clowder

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import java.io.File
import java.util.concurrent.atomic.AtomicBoolean

/**
 * Runs the clowder daemon as a child process, kept alive by the
 * foreground notification plus a partial wake lock (Android kills the
 * daemon without one — see AGENTS). The daemon binary ships as
 * libclowder.so in jniLibs (useLegacyPackaging extracts it to
 * nativeLibraryDir, which is executable) and is a plain GOOS=android
 * build: tailcat's android_linux.go patches DNS, CA certs and
 * interface discovery at init.
 *
 * The app talks to the daemon over the same unix-socket IPC the clow
 * CLI uses; nothing here touches the daemon package.
 */
class ClowdService : Service() {

    companion object {
        const val ACTION_START = "cloud.terence.clowder.START"
        const val ACTION_STOP = "cloud.terence.clowder.STOP"
        private const val CHANNEL_ID = "clowder-daemon"
        private const val NOTIFICATION_ID = 1
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

        fun configDir(ctx: Context): File = File(ctx.filesDir, "clowder")
        fun socketFile(ctx: Context): File = File(configDir(ctx), "clow.sock")
        fun inboxDir(ctx: Context): File = File(ctx.filesDir, "Downloads/clowder")
        fun binaryFile(ctx: Context): File =
            File(ctx.applicationInfo.nativeLibraryDir, BINARY)

        fun isInitialized(ctx: Context): Boolean =
            File(configDir(ctx), "me.json").isFile

        fun start(ctx: Context) {
            ctx.startForegroundService(Intent(ctx, ClowdService::class.java).setAction(ACTION_START))
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, ClowdService::class.java).setAction(ACTION_STOP))
        }
    }

    private var wakeLock: PowerManager.WakeLock? = null
    private var daemon: Process? = null
    private val stopping = AtomicBoolean(false)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopping.set(true)
                killDaemon()
                Log.i(TAG, "daemon stopped")
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                return START_NOT_STICKY
            }
            else -> startForegroundServiceNow()
        }
        return START_STICKY
    }

    private fun startForegroundServiceNow() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "clowder daemon", NotificationManager.IMPORTANCE_LOW)
        )
        val notification: Notification = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("clowder")
            .setContentText("daemon running — receiving files")
            .setSmallIcon(android.R.drawable.stat_notify_sync_noanim)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 29) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }

        wakeLock = getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "clowder:daemon").apply {
                setReferenceCounted(false)
                acquire()
            }

        if (daemon?.isAlive == true) return
        stopping.set(false)
        Thread { runDaemon() }.start()
    }

    /** Runs the daemon, restarting on unexpected exits with a simple
     *  backoff; a crash loop must not spin the CPU. */
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
                appendLog("daemon started (pid ${proc.pid ?: "?"})")
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
        wakeLock?.let { if (it.isHeld) it.release() }
        wakeLock = null
    }

    override fun onDestroy() {
        stopping.set(true)
        killDaemon()
        super.onDestroy()
    }
}
