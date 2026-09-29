package cloud.terence.clowder

import android.content.ContentUris
import android.content.Intent
import android.net.LocalSocketAddress
import android.net.LocalSocket
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.FileObserver
import android.provider.MediaStore
import android.text.format.DateUtils
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Add
import androidx.compose.material.icons.outlined.Close
import androidx.compose.material.icons.outlined.Email
import androidx.compose.material.icons.outlined.KeyboardArrowLeft
import androidx.compose.material.icons.outlined.KeyboardArrowRight
import androidx.compose.material.icons.outlined.PlayArrow
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.core.content.FileProvider
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            ClowderTheme { ClowderApp() }
        }
    }

    // The daemon runs only while the app is on screen: started when
    // the activity becomes visible, stopped when it does not.
    override fun onStart() {
        super.onStart()
        if (ClowdService.isInitialized(this)) ClowdService.start(this)
    }

    override fun onStop() {
        ClowdService.stop(this)
        super.onStop()
    }
}

/** A pushed screen; null is home. Each is a full-screen overlay —
 *  the home list underneath stops composing (and polling). */
private sealed interface Screen {
    data class Cat(val cat: cloud.terence.clowder.Cat) : Screen
    data object Pairing : Screen
    data object Inbox : Screen
    data object Settings : Screen
    data object Log : Screen
}

/** A pushed screen's short swipe: in from the right, out the same
 *  way, 200 ms. */
private val SwipeIn =
    slideInHorizontally(animationSpec = tween(200)) { it } + fadeIn(tween(200))
private val SwipeOut =
    slideOutHorizontally(animationSpec = tween(200)) { it } + fadeOut(tween(200))

@Composable
fun ClowderApp() {
    val ctx = LocalContext.current
    var initialized by remember { mutableStateOf(ClowdService.isInitialized(ctx)) }

    if (!initialized) {
        InitScreen { initialized = true }
        return
    }

    var screen by remember { mutableStateOf<Screen?>(null) }
    BackHandler(enabled = screen != null) { screen = null }
    Scaffold { padding ->
        Box(Modifier.padding(padding)) {
            when (val s = screen) {
                null -> HomeScreen(
                    onCat = { screen = Screen.Cat(it) },
                    onPair = { screen = Screen.Pairing },
                    onInbox = { screen = Screen.Inbox },
                    onSettings = { screen = Screen.Settings },
                )
                else -> {}
            }
            AnimatedVisibility(visible = screen is Screen.Cat, enter = SwipeIn, exit = SwipeOut) {
                (screen as? Screen.Cat)?.let {
                    CatScreen(it.cat, onClose = { screen = null })
                }
            }
            AnimatedVisibility(visible = screen is Screen.Pairing, enter = SwipeIn, exit = SwipeOut) {
                PairScreen(onClose = { screen = null })
            }
            AnimatedVisibility(visible = screen is Screen.Inbox, enter = SwipeIn, exit = SwipeOut) {
                InboxScreen(onClose = { screen = null })
            }
            AnimatedVisibility(visible = screen is Screen.Settings, enter = SwipeIn, exit = SwipeOut) {
                SettingsScreen(
                    onClose = { screen = null },
                    onShowLog = { screen = Screen.Log },
                    // A reset that succeeds lands on init; everything
                    // above stops composing because initialized flips
                    // false first.
                    onReset = { initialized = false },
                )
            }
            AnimatedVisibility(visible = screen is Screen.Log, enter = SwipeIn, exit = SwipeOut) {
                LogScreen(onClose = { screen = Screen.Settings })
            }
        }
    }
}

/** `clow init <name>` runs the binary directly (init is a local file
 *  op; the daemon is not involved). */
private fun runInit(ctx: android.content.Context, name: String): String = try {
    val dir = ClowdService.configDir(ctx)
    if (!dir.isDirectory) dir.mkdirs()
    val proc = ProcessBuilder(ClowdService.binaryFile(ctx).absolutePath, "init", "--name", name)
        .redirectErrorStream(true)
        .apply {
            environment()["CLOWDER_DIR"] = dir.absolutePath
            environment()["HOME"] = ctx.filesDir.absolutePath
        }
        .start()
    proc.inputStream.bufferedReader().readText() + if (proc.waitFor() == 0) "" else " (init failed)"
} catch (e: Exception) {
    "init failed: ${e.message}"
}

@Composable
fun InitScreen(onDone: () -> Unit) {
    val ctx = LocalContext.current
    val scope = rememberCoroutineScope()
    var name by remember { mutableStateOf("") }
    var message by remember { mutableStateOf<String?>(null) }

    Column(
        Modifier
            .fillMaxSize()
            .padding(32.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text("clowder", style = MaterialTheme.typography.displaySmall)
        Text(
            "Name this cat. The name is the human handle inside a clowder.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedTextField(
            value = name,
            onValueChange = { name = it },
            label = { Text("cat name") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Button(
            onClick = {
                scope.launch {
                    val out = withContext(Dispatchers.IO) { runInit(ctx, name.trim()) }
                    if (ClowdService.isInitialized(ctx)) {
                        ClowdService.start(ctx)
                        onDone()
                    } else {
                        message = out
                    }
                }
            },
            enabled = name.isNotBlank(),
        ) { Text("init and start daemon") }
        message?.let { Text(it, color = MaterialTheme.colorScheme.error) }
    }
}

/** `clow reset --yes` as the app runs it: announce the leave first (a
 *  reset cat departs the clowder — the same op the CLI's reset drives,
 *  sent while the daemon can still reach anyone), then stop the daemon
 *  service and wait until its IPC socket stops answering (reset refuses
 *  while the daemon is up), then exec the binary against the same
 *  sandboxed config dir init uses. */
private fun runReset(ctx: android.content.Context): String = try {
    runCatching { ipc(ClowdService.socketFile(ctx), "leave") }
    ClowdService.stop(ctx)
    val sock = ClowdService.socketFile(ctx).absolutePath
    for (i in 0 until 40) {
        val alive = runCatching {
            LocalSocket().use { s ->
                s.connect(LocalSocketAddress(sock, LocalSocketAddress.Namespace.FILESYSTEM))
            }
        }.isSuccess
        if (!alive) break
        Thread.sleep(250)
    }
    val proc = ProcessBuilder(ClowdService.binaryFile(ctx).absolutePath, "reset", "--yes")
        .redirectErrorStream(true)
        .apply {
            environment()["CLOWDER_DIR"] = ClowdService.configDir(ctx).absolutePath
            environment()["HOME"] = ctx.filesDir.absolutePath
        }
        .start()
    proc.inputStream.bufferedReader().readText() + if (proc.waitFor() == 0) "" else " (reset failed)"
} catch (e: Exception) {
    "reset failed: ${e.message}"
}

@Composable
private fun SectionHeader(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleSmall,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier.padding(top = 16.dp, bottom = 4.dp),
    )
}

@Composable
private fun LivenessDot(online: Boolean) {
    Box(
        Modifier
            .size(10.dp)
            .clip(CircleShape)
            .background(if (online) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant),
    )
}

/** Home: the cat list. Sending is the point of the app, so the cats
 *  are the list, and everything else (inbox, pair, settings) hangs
 *  off the top bar; a cat's detail screen carries the send action. */
@Composable
private fun HomeScreen(
    onCat: (Cat) -> Unit,
    onPair: () -> Unit,
    onInbox: () -> Unit,
    onSettings: () -> Unit,
) {
    val ctx = LocalContext.current
    var status by remember { mutableStateOf<Status?>(null) }
    var notice by remember { mutableStateOf<String?>(null) }
    var noticeIsError by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        while (true) {
            val s = withContextOrNull { parseStatus(ipc(ClowdService.socketFile(ctx), "status")) }
            if (s == null) {
                // A poll miss while the daemon process is alive is just
                // startup: say so instead of crying unreachable.
                noticeIsError = !ClowdService.running
                notice = if (ClowdService.running) "daemon starting…" else "daemon not reachable"
                status = null
            } else {
                notice = if (s.ok) null else s.error
                noticeIsError = true
                status = s
            }
            delay(3000)
        }
    }

    LazyColumn(
        Modifier
            .fillMaxSize()
            .padding(horizontal = 16.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        item {
            Row(
                Modifier.padding(top = 16.dp, bottom = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(4.dp),
            ) {
                LivenessDot(status?.ok == true)
                Text("clowder", style = MaterialTheme.typography.titleLarge)
                Spacer(Modifier.weight(1f))
                IconButton(onClick = onInbox) {
                    Icon(Icons.Outlined.Email, contentDescription = "inbox")
                }
                IconButton(onClick = onPair) {
                    Icon(Icons.Outlined.Add, contentDescription = "pair a cat")
                }
                IconButton(onClick = onSettings) {
                    Icon(Icons.Outlined.Settings, contentDescription = "settings")
                }
            }
            notice?.let {
                ListItem(
                    headlineContent = {
                        Text(
                            it,
                            color = if (noticeIsError) {
                                MaterialTheme.colorScheme.error
                            } else {
                                MaterialTheme.colorScheme.onSurfaceVariant
                            },
                        )
                    },
                )
            }
        }

        val st = status
        if (st == null || !st.ok) return@LazyColumn

        item {
            ElevatedCard(Modifier.fillMaxWidth()) {
                ListItem(
                    headlineContent = { Text("me: ${st.me?.name ?: "?"}", style = MaterialTheme.typography.titleMedium) },
                    supportingContent = {
                        Text(
                            "sent ${st.sentFiles} (${humanBytes(st.sentBytes)}) · " +
                                "received ${st.receivedFiles} (${humanBytes(st.receivedBytes)})",
                        )
                    },
                    leadingContent = { LivenessDot(true) },
                )
            }
        }

        item { SectionHeader("cats · ${st.cats.size}") }
        if (st.cats.isEmpty()) {
            item {
                Column(horizontalAlignment = Alignment.CenterHorizontally, modifier = Modifier.fillMaxWidth()) {
                    Text(
                        "no cats yet — pair one to start sending",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(vertical = 8.dp),
                    )
                    FilledTonalButton(onClick = onPair) { Text("pair a cat") }
                }
            }
        }
        items(st.cats) { c ->
            val seen = st.liveness[c.key]?.takeIf { it != 0L }
            val online = seen != null && System.currentTimeMillis() / 1000 - seen < 120
            ListItem(
                modifier = Modifier.clickable { onCat(c) },
                headlineContent = { Text(c.name) },
                supportingContent = {
                    val role = when {
                        c.dropbox -> " · dropbox"
                        c.storer -> " · storer"
                        else -> ""
                    }
                    Text(livenessText(seen) + role)
                },
                leadingContent = { LivenessDot(online) },
            )
        }

        if (st.transfers.isNotEmpty()) {
            item { SectionHeader("transfers") }
            items(st.transfers) { t ->
                ListItem(
                    headlineContent = { Text("${if (t.receiving) "receiving" else "sending"} ${t.fileName}") },
                    supportingContent = {
                        Column {
                            Text("${t.peer} · ${100 * t.done / t.total.coerceAtLeast(1)}%")
                            LinearProgressIndicator(
                                progress = { t.done.toFloat() / t.total.coerceAtLeast(1).toFloat() },
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .padding(top = 4.dp),
                            )
                        }
                    },
                )
            }
        }

        if (st.outbox.isNotEmpty()) {
            item { SectionHeader("outbox · ${st.outbox.size} pending") }
            items(st.outbox) { e ->
                ListItem(
                    headlineContent = { Text(e.fileName) },
                    supportingContent = { Text("queued for ${e.targetName}") },
                )
            }
        }
    }
}

/** One cat: the send action, what is moving to/from it right now,
 *  what is queued for it, and forget. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun CatScreen(cat: Cat, onClose: () -> Unit) {
    val ctx = LocalContext.current
    var status by remember { mutableStateOf<Status?>(null) }
    var sendResult by remember { mutableStateOf<String?>(null) }
    var pingResult by remember { mutableStateOf<String?>(null) }
    var forgetConfirm by remember { mutableStateOf(false) }
    var forgetError by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    val canSend = !cat.storer && !cat.dropbox

    LaunchedEffect(Unit) {
        while (true) {
            status = withContextOrNull { parseStatus(ipc(ClowdService.socketFile(ctx), "status")) }
            delay(3000)
        }
    }

    val pickFile = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) {
            scope.launch {
                sendResult = withContext(Dispatchers.IO) { sendUri(ctx, uri, cat.name) }
            }
        }
    }

    if (forgetConfirm) {
        AlertDialog(
            onDismissRequest = { forgetConfirm = false },
            title = { Text("Forget ${cat.name}?") },
            text = {
                Text(
                    "Drops ${cat.name} from your roster and cancels any pending " +
                        "sends to it. Local only: ${cat.name} keeps you in its roster.",
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    forgetConfirm = false
                    scope.launch {
                        val r = withContextOrNull {
                            ipc(ClowdService.socketFile(ctx), "forget", target = cat.name)
                        }
                        if (r == null || !r.optBoolean("ok")) {
                            forgetError = r?.optString("error")?.ifEmpty { null } ?: "daemon not reachable"
                        } else {
                            onClose()
                        }
                    }
                }) { Text("Forget", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = {
                TextButton(onClick = { forgetConfirm = false }) { Text("Cancel") }
            },
        )
    }

    Surface(Modifier.fillMaxSize()) {
        Column(Modifier.fillMaxSize().padding(horizontal = 16.dp)) {
            Row(
                Modifier.padding(top = 16.dp, bottom = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onClose) {
                    Icon(Icons.Outlined.KeyboardArrowLeft, contentDescription = "back")
                }
                Text(cat.name, style = MaterialTheme.typography.titleLarge)
                Spacer(Modifier.weight(1f))
                Text(
                    cat.key.take(8),
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            HorizontalDivider(Modifier.padding(bottom = 8.dp))

            val seen = status?.liveness?.get(cat.key)?.takeIf { it != 0L }
            val online = seen != null && System.currentTimeMillis() / 1000 - seen < 120
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                LivenessDot(online)
                Text(
                    livenessText(seen) +
                        when {
                            cat.dropbox -> " · dropbox"
                            cat.storer -> " · storer"
                            else -> ""
                        },
                    style = MaterialTheme.typography.bodyMedium,
                    modifier = Modifier.weight(1f),
                )
                TextButton(onClick = {
                    scope.launch {
                        val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), "ping", target = cat.name) }
                        pingResult = when {
                            r == null -> "daemon not reachable"
                            r.optBoolean("ok", false) -> r.optString("message")
                            else -> r.optString("error")
                        }
                    }
                }) { Text("ping") }
            }
            pingResult?.let {
                Text(it, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(top = 4.dp))
            }
            if (canSend) {
                Button(
                    onClick = { pickFile.launch(arrayOf("*/*")) },
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(top = 16.dp),
                ) { Text("send a file to ${cat.name}") }
            } else {
                Text(
                    "storers and dropboxes hold files — send to a regular cat",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 16.dp),
                )
            }
            sendResult?.let {
                Text(it, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(top = 8.dp))
            }
            forgetError?.let {
                Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
            }

            val transfers = status?.transfers?.filter { it.peer == cat.name } ?: emptyList()
            if (transfers.isNotEmpty()) {
                SectionHeader("transfers")
                transfers.forEach { t ->
                    ListItem(
                        headlineContent = { Text("${if (t.receiving) "receiving" else "sending"} ${t.fileName}") },
                        supportingContent = {
                            Column {
                                Text("${100 * t.done / t.total.coerceAtLeast(1)}%")
                                LinearProgressIndicator(
                                    progress = { t.done.toFloat() / t.total.coerceAtLeast(1).toFloat() },
                                    modifier = Modifier
                                        .fillMaxWidth()
                                        .padding(top = 4.dp),
                                )
                            }
                        },
                    )
                }
            }

            val outbox = status?.outbox?.filter { it.targetName == cat.name } ?: emptyList()
            if (outbox.isNotEmpty()) {
                SectionHeader("queued for ${cat.name} · ${outbox.size}")
                outbox.forEach { e ->
                    ListItem(
                        headlineContent = { Text(e.fileName) },
                        supportingContent = { Text("waiting to deliver") },
                    )
                }
            }

            Spacer(Modifier.weight(1f))
            TextButton(
                onClick = { forgetConfirm = true },
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(bottom = 24.dp),
            ) { Text("Forget ${cat.name}", color = MaterialTheme.colorScheme.error) }
        }
    }
}

@Composable
private fun PairScreen(onClose: () -> Unit) {
    val ctx = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    var inviteMessage by remember { mutableStateOf<String?>(null) }
    var joinCode by remember { mutableStateOf("") }
    var joinResult by remember { mutableStateOf<String?>(null) }

    Surface(Modifier.fillMaxSize()) {
        Column(Modifier.fillMaxSize().padding(horizontal = 16.dp)) {
            Row(
                Modifier.padding(top = 16.dp, bottom = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onClose) {
                    Icon(Icons.Outlined.KeyboardArrowLeft, contentDescription = "back")
                }
                Text("pair a cat", style = MaterialTheme.typography.titleLarge)
            }
            HorizontalDivider(Modifier.padding(bottom = 8.dp))
            Column(
                Modifier
                    .fillMaxSize()
                    .padding(top = 16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                SectionHeader("invite")
                Text(
                    "Generates a fresh one-off code, valid 5 minutes. Read it to the other cat.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                FilledTonalButton(onClick = {
                    scope.launch {
                        val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), "invite") }
                        inviteMessage = if (r != null && r.optBoolean("ok", false)) {
                            r.optString("message").removePrefix("ask the other cat to run: clow join ")
                        } else {
                            r?.optString("error") ?: "daemon not reachable"
                        }
                    }
                }) { Text("new invite code") }

                inviteMessage?.let { code ->
                    ElevatedCard(Modifier.fillMaxWidth()) {
                        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                            Text(
                                code,
                                fontFamily = FontFamily.Monospace,
                                style = MaterialTheme.typography.titleMedium,
                            )
                            TextButton(onClick = { clipboard.setText(AnnotatedString(code)) }) { Text("copy code") }
                        }
                    }
                }

                HorizontalDivider(Modifier.padding(top = 8.dp))

                SectionHeader("join")
                OutlinedTextField(
                    value = joinCode,
                    onValueChange = { joinCode = it },
                    label = { Text("pairing code (8 words + region)") },
                    modifier = Modifier.fillMaxWidth(),
                )
                Button(
                    onClick = {
                        scope.launch {
                            val r = withContextOrNull {
                                ipc(ClowdService.socketFile(ctx), words = joinCode.trim(), op = "join")
                            }
                            when {
                                r == null -> joinResult = "daemon not reachable"
                                // A paired cat shows on home within a poll tick.
                                r.optBoolean("ok", false) -> onClose()
                                else -> joinResult = r.optString("error")
                            }
                        }
                    },
                    enabled = joinCode.isNotBlank(),
                ) { Text("join") }
                joinResult?.let { Text(it) }
            }
        }
    }
}

/** Copies the picked SAF document into the sandbox (the daemon needs a
 *  real path, not a content URI) and queues the send. */
private fun sendUri(ctx: android.content.Context, uri: Uri, target: String): String = try {
    val name = queryName(ctx, uri) ?: "file"
    val staging = File(ctx.cacheDir, name)
    ctx.contentResolver.openInputStream(uri)!!.use { input ->
        staging.outputStream().use { input.copyTo(it) }
    }
    val r = ipc(ClowdService.socketFile(ctx), op = "send", target = target, path = staging.absolutePath)
    if (r.optBoolean("ok", false)) r.optString("message") else r.optString("error")
} catch (e: Exception) {
    "send failed: ${e.message}"
}

private fun queryName(ctx: android.content.Context, uri: Uri): String? {
    if (uri.scheme == "file") return File(uri.path ?: return null).name
    ctx.contentResolver.query(uri, null, null, null, null)?.use { c ->
        if (c.moveToFirst()) {
            val idx = c.getColumnIndex(android.provider.OpenableColumns.DISPLAY_NAME)
            if (idx >= 0) return c.getString(idx)
        }
    }
    return null
}

/** One inbox row: either a MediaStore Downloads item (the publisher
 *  moves arrivals there on API 29+) or a sandbox file below that. */
data class InboxItem(
    val uri: Uri?,
    val file: File?,
    val name: String,
    val size: Long,
    val modified: Long,
)

@Composable
fun InboxScreen(onClose: () -> Unit) {
    val ctx = LocalContext.current
    var items by remember { mutableStateOf(listOf<InboxItem>()) }
    var refreshKey by remember { mutableIntStateOf(0) }

    LaunchedEffect(refreshKey) {
        items = withContext(Dispatchers.IO) {
            if (Build.VERSION.SDK_INT >= 29) {
                val out = mutableListOf<InboxItem>()
                ctx.contentResolver.query(
                    MediaStore.Downloads.EXTERNAL_CONTENT_URI,
                    arrayOf(
                        MediaStore.MediaColumns._ID,
                        MediaStore.MediaColumns.DISPLAY_NAME,
                        MediaStore.MediaColumns.SIZE,
                        MediaStore.MediaColumns.DATE_ADDED,
                    ),
                    "${MediaStore.MediaColumns.RELATIVE_PATH}=?",
                    // MediaProvider stores the bucket WITH a trailing
                    // slash; matching without it lists nothing.
                    arrayOf("Download/clowder/"),
                    "${MediaStore.MediaColumns.DATE_ADDED} DESC",
                )?.use { c ->
                    while (c.moveToNext()) {
                        out += InboxItem(
                            uri = ContentUris.withAppendedId(
                                MediaStore.Downloads.EXTERNAL_CONTENT_URI, c.getLong(0),
                            ),
                            file = null,
                            name = c.getString(1),
                            size = c.getLong(2),
                            modified = c.getLong(3) * 1000,
                        )
                    }
                }
                out
            } else {
                ClowdService.inboxDir(ctx).listFiles()
                    ?.sortedByDescending { it.lastModified() }
                    ?.map { InboxItem(null, it, it.name, it.length(), it.lastModified()) }
                    ?: emptyList()
            }
        }
    }

    // The daemon lands files in the sandbox inbox; the service's
    // publisher moves them into Downloads. Watch the sandbox — arrival
    // is the refresh trigger either way. (The deprecated String
    // constructor covers minSdk 26; the File one is API 29+.)
    DisposableEffect(Unit) {
        val dir = ClowdService.inboxDir(ctx)
        if (!dir.isDirectory) dir.mkdirs()
        @Suppress("DEPRECATION")
        val observer = object : FileObserver(
            dir.path,
            FileObserver.CLOSE_WRITE or FileObserver.MOVED_TO or
                FileObserver.MOVED_FROM or FileObserver.DELETE,
        ) {
            override fun onEvent(event: Int, path: String?) {
                refreshKey++
            }
        }
        observer.startWatching()
        onDispose { observer.stopWatching() }
    }

    Column(Modifier.fillMaxSize()) {
        Row(
            Modifier
                .fillMaxWidth()
                .padding(start = 12.dp, end = 24.dp, top = 16.dp, bottom = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            IconButton(onClick = onClose) {
                Icon(Icons.Outlined.KeyboardArrowLeft, contentDescription = "back")
            }
            Text("inbox", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
            OutlinedButton(onClick = { openInFilesApp(ctx) }) { Text("open in files") }
        }
        if (items.isEmpty()) {
            Text(
                "nothing received yet",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(horizontal = 24.dp),
            )
        }
        LazyColumn {
            items(items) { f ->
                ListItem(
                    headlineContent = { Text(f.name) },
                    supportingContent = {
                        Text(
                            "${humanBytes(f.size)} · " +
                                DateUtils.getRelativeTimeSpanString(f.modified).toString(),
                        )
                    },
                    modifier = Modifier.fillMaxWidth(),
                    trailingContent = {
                        TextButton(onClick = { openInboxItem(ctx, f) }) { Text("open") }
                    },
                )
            }
        }
    }
}

private fun openInboxItem(ctx: android.content.Context, item: InboxItem) {
    val uri = item.uri ?: FileProvider.getUriForFile(ctx, "${ctx.packageName}.files", item.file!!)
    val intent = Intent(Intent.ACTION_VIEW)
        .setDataAndType(uri, ctx.contentResolver.getType(uri) ?: "application/octet-stream")
        .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    runCatching { ctx.startActivity(Intent.createChooser(intent, item.name)) }
}

/** Opens the phone's file manager. DocumentsUI is the platform's
 *  (AOSP and Pixel package names); received files live in the real
 *  Download/clowder directory, so any file manager shows them.
 *  Without a Files app at all, fall back to the downloads UI. */
private fun openInFilesApp(ctx: android.content.Context) {
    for (pkg in listOf("com.android.documentsui", "com.google.android.documentsui")) {
        val intent = ctx.packageManager.getLaunchIntentForPackage(pkg)
        if (intent != null) {
            runCatching { ctx.startActivity(intent) }
            return
        }
    }
    runCatching {
        ctx.startActivity(Intent(android.app.DownloadManager.ACTION_VIEW_DOWNLOADS))
    }
}

/** Settings as a full screen, not a popup: daemon lifecycle, the
 *  daemon log, leave and reset. Each keeps its confirmation dialog —
 *  a destructive action should interrupt. */
@Composable
fun SettingsScreen(onClose: () -> Unit, onShowLog: () -> Unit, onReset: () -> Unit) {
    val ctx = LocalContext.current
    var showReset by remember { mutableStateOf(false) }
    var resetResult by remember { mutableStateOf<String?>(null) }
    var showLeave by remember { mutableStateOf(false) }
    var leaveResult by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

    if (showReset) {
        AlertDialog(
            onDismissRequest = { showReset = false },
            title = { Text("Reset this cat?") },
            text = {
                Text(
                    "Its identity, rosters, spool and outbox are deleted and " +
                        "you will name a new cat. Received files are kept. " +
                        "This cannot be undone.",
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    showReset = false
                    scope.launch {
                        val out = withContext(Dispatchers.IO) { runReset(ctx) }
                        if (ClowdService.isInitialized(ctx)) {
                            resetResult = out
                        } else {
                            onReset()
                        }
                    }
                }) { Text("Reset", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = {
                TextButton(onClick = { showReset = false }) { Text("Cancel") }
            },
        )
    }
    resetResult?.let {
        AlertDialog(
            onDismissRequest = { resetResult = null },
            title = { Text("reset failed") },
            text = { Text(it) },
            confirmButton = {
                TextButton(onClick = { resetResult = null }) { Text("ok") }
            },
        )
    }

    if (showLeave) {
        AlertDialog(
            onDismissRequest = { showLeave = false },
            title = { Text("Leave this clowder?") },
            text = {
                Text(
                    "Says a signed goodbye to every reachable cat, then wipes " +
                        "your roster, spool and outbox. Your identity is kept: " +
                        "pair again with invite or join.",
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    showLeave = false
                    scope.launch {
                        // The announce can take up to its 10s budget.
                        val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), "leave") }
                        leaveResult = when {
                            r == null -> "daemon not reachable"
                            r.optBoolean("ok", false) -> r.optString("message")
                            else -> r.optString("error")
                        }
                    }
                }) { Text("Leave", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = {
                TextButton(onClick = { showLeave = false }) { Text("Cancel") }
            },
        )
    }
    leaveResult?.let {
        AlertDialog(
            onDismissRequest = { leaveResult = null },
            title = { Text("left the clowder") },
            text = { Text(it) },
            confirmButton = {
                TextButton(onClick = { leaveResult = null }) { Text("ok") }
            },
        )
    }

    Surface(Modifier.fillMaxSize()) {
        Column(Modifier.fillMaxSize().padding(horizontal = 16.dp)) {
            Row(
                Modifier.padding(top = 16.dp, bottom = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onClose) {
                    Icon(Icons.Outlined.KeyboardArrowLeft, contentDescription = "back")
                }
                Text("settings", style = MaterialTheme.typography.titleLarge)
            }
            HorizontalDivider(Modifier.padding(bottom = 8.dp))
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                LivenessDot(ClowdService.running)
                Text(
                    if (ClowdService.running) "daemon running" else "daemon stopped",
                    style = MaterialTheme.typography.titleSmall,
                )
                Spacer(Modifier.weight(1f))
                if (ClowdService.running) {
                    OutlinedButton(onClick = { ClowdService.stop(ctx) }) { Text("Stop") }
                } else {
                    FilledTonalButton(onClick = { ClowdService.start(ctx) }) {
                        Icon(Icons.Outlined.PlayArrow, contentDescription = null)
                        Text("Start")
                    }
                }
            }
            HorizontalDivider(Modifier.padding(vertical = 8.dp))
            MenuRow("View daemon log") { onShowLog() }
            HorizontalDivider()
            MenuRow("Clear daemon log") { ClowdService.clearLog() }
            HorizontalDivider()
            MenuRow("Leave the clowder…", danger = true) { showLeave = true }
            HorizontalDivider()
            MenuRow("Reset this cat…", danger = true) { showReset = true }
            HorizontalDivider()
        }
    }
}

/** One settings menu row: label, chevron, whole row clickable. */
@Composable
private fun MenuRow(label: String, danger: Boolean = false, onClick: () -> Unit) {
    Row(
        Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(vertical = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            label,
            style = MaterialTheme.typography.bodyLarge,
            color = if (danger) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
            modifier = Modifier.weight(1f),
        )
        Icon(
            Icons.Outlined.KeyboardArrowRight,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
fun LogScreen(onClose: () -> Unit) {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant,
        modifier = Modifier.fillMaxSize(),
    ) {
        var follow by remember { mutableStateOf(true) }
        var log by remember { mutableStateOf(ClowdService.recentLog()) }
        val listState = rememberLazyListState()
        val scope = rememberCoroutineScope()

        Column(Modifier.fillMaxSize()) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.padding(start = 16.dp, end = 4.dp),
            ) {
                Text(
                    "daemon log",
                    style = MaterialTheme.typography.titleSmall,
                    modifier = Modifier.weight(1f),
                )
                Text("autoscroll", style = MaterialTheme.typography.bodySmall)
                Checkbox(checked = follow, onCheckedChange = { on ->
                    follow = on
                    if (on) {
                        scope.launch {
                            val n = listState.layoutInfo.totalItemsCount
                            if (n > 0) listState.scrollToItem(n - 1)
                        }
                    }
                })
                IconButton(onClick = onClose) {
                    Icon(Icons.Outlined.Close, contentDescription = "Close")
                }
            }
            HorizontalDivider()
            SelectionContainer {
                // Poll the service's ring buffer; while autoscroll is
                // checked, pin to the newest line.
                LaunchedEffect(Unit) {
                    while (true) {
                        log = ClowdService.recentLog()
                        if (follow) {
                            val n = listState.layoutInfo.totalItemsCount
                            if (n > 0) listState.scrollToItem(n - 1)
                        }
                        delay(1000)
                    }
                }
                LazyColumn(
                    state = listState,
                    modifier = Modifier
                        .weight(1f)
                        .padding(12.dp),
                ) {
                    items(if (log.isEmpty()) emptyList() else log.split("\n")) { line ->
                        Text(
                            line,
                            fontFamily = FontFamily.Monospace,
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
            }
        }
    }
}

/** Runs [block] on Dispatchers.IO, returning null on any failure (a
 *  socket error means the daemon is down; the UI treats null as
 *  unreachable). */
private suspend fun <T> withContextOrNull(block: () -> T): T? = withContext(Dispatchers.IO) {
    runCatching { block() }.getOrNull()
}
