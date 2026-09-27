package cloud.terence.clowder

import android.content.Intent
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.net.Uri
import android.os.Bundle
import android.os.FileObserver
import android.text.format.DateUtils
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
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
import androidx.compose.material.icons.outlined.Email
import androidx.compose.material.icons.outlined.Home
import androidx.compose.material.icons.outlined.Info
import androidx.compose.material.icons.outlined.KeyboardArrowDown
import androidx.compose.material.icons.outlined.PlayArrow
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.snapshotFlow
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
}

private data class Tab(val label: String, val icon: androidx.compose.ui.graphics.vector.ImageVector)

@Composable
fun ClowderApp() {
    val ctx = LocalContext.current
    var initialized by remember { mutableStateOf(ClowdService.isInitialized(ctx)) }

    if (!initialized) {
        InitScreen { initialized = true }
        return
    }

    val tabs = listOf(
        Tab("Status", Icons.Outlined.Home),
        Tab("Pair", Icons.Outlined.Add),
        Tab("Send", Icons.Outlined.Share),
        Tab("Inbox", Icons.Outlined.Email),
        Tab("Log", Icons.Outlined.Info),
    )
    var tab by remember { mutableIntStateOf(0) }
    // The daemon is the app: it starts with the UI (this effect
    // composes once the cat is initialized) and is stopped from the
    // settings dialog.
    LaunchedEffect(Unit) {
        if (!ClowdService.running) ClowdService.start(ctx)
    }
    Scaffold(
        bottomBar = {
            NavigationBar {
                tabs.forEachIndexed { i, t ->
                    NavigationBarItem(
                        selected = tab == i,
                        onClick = { tab = i },
                        label = { Text(t.label) },
                        icon = { Icon(t.icon, contentDescription = t.label) },
                    )
                }
            }
        },
    ) { padding ->
        Box(Modifier.padding(padding)) {
            when (tab) {
                0 -> StatusScreen(onReset = { initialized = false })
                1 -> PairScreen()
                2 -> SendScreen()
                3 -> InboxScreen()
                4 -> LogScreen()
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

/** `clow reset --yes` as the app runs it: stop the daemon service
 *  first and wait until its IPC socket stops answering (reset refuses
 *  while the daemon is up), then exec the binary against the same
 *  sandboxed config dir init uses. */
private fun runReset(ctx: android.content.Context): String = try {
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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun StatusScreen(onReset: () -> Unit) {
    val ctx = LocalContext.current
    var status by remember { mutableStateOf<Status?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var showSettings by remember { mutableStateOf(false) }
    var showReset by remember { mutableStateOf(false) }
    var resetResult by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

    if (showSettings) {
        AlertDialog(
            onDismissRequest = { showSettings = false },
            title = { Text("Settings") },
            text = {
                Column {
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
                            OutlinedButton(onClick = {
                                ClowdService.stop(ctx)
                                showSettings = false
                            }) { Text("Stop") }
                        } else {
                            FilledTonalButton(onClick = {
                                ClowdService.start(ctx)
                                showSettings = false
                            }) {
                                Icon(Icons.Outlined.PlayArrow, contentDescription = null)
                                Text("Start")
                            }
                        }
                    }
                    HorizontalDivider(Modifier.padding(vertical = 8.dp))
                    TextButton(onClick = {
                        ClowdService.clearLog()
                        showSettings = false
                    }) { Text("Clear daemon log") }
                    TextButton(onClick = {
                        showSettings = false
                        showReset = true
                    }) { Text("Reset this cat\u2026", color = MaterialTheme.colorScheme.error) }
                }
            },
            confirmButton = {
                TextButton(onClick = { showSettings = false }) { Text("Done") }
            },
        )
    }

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

    LaunchedEffect(Unit) {
        while (true) {
            val s = withContextOrNull { parseStatus(ipc(ClowdService.socketFile(ctx), "status")) }
            if (s == null) {
                error = "daemon not reachable"
                status = null
            } else {
                error = if (s.ok) null else s.error
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
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                LivenessDot(ClowdService.running)
                Text(
                    if (ClowdService.running) "daemon running" else "daemon stopped",
                    style = MaterialTheme.typography.titleMedium,
                )
                Spacer(Modifier.weight(1f))
                IconButton(onClick = { showSettings = true }) {
                    Icon(Icons.Outlined.Settings, contentDescription = "settings")
                }
            }
            error?.let {
                ListItem(
                    headlineContent = { Text(it, color = MaterialTheme.colorScheme.error) },
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

        item { SectionHeader("clowder · ${st.cats.size} cats") }
        if (st.cats.isEmpty()) {
            item {
                Text(
                    "no cats yet — pair one from the Pair tab",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        items(st.cats) { c ->
            val seen = st.liveness[c.key]?.takeIf { it != 0L }
            val path = st.paths[c.key]
            ListItem(
                headlineContent = { Text(c.name) },
                supportingContent = {
                    val role = when {
                        c.dropbox -> " · dropbox"
                        c.storer -> " · storer"
                        else -> ""
                    }
                    val route = when {
                        path == null -> ""
                        path.direct -> " · direct"
                        else -> " · relayed"
                    }
                    Text(livenessText(seen) + role + route)
                },
                leadingContent = {
                    LivenessDot(seen != null && System.currentTimeMillis() / 1000 - seen < 120)
                },
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

@Composable
fun PairScreen() {
    val ctx = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    var inviteMessage by remember { mutableStateOf<String?>(null) }
    var joinCode by remember { mutableStateOf("") }
    var joinResult by remember { mutableStateOf<String?>(null) }

    Column(
        Modifier
            .fillMaxSize()
            .padding(24.dp),
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
                    val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), words = joinCode.trim(), op = "join") }
                    joinResult = when {
                        r == null -> "daemon not reachable"
                        r.optBoolean("ok", false) -> "paired"
                        else -> r.optString("error")
                    }
                }
            },
            enabled = joinCode.isNotBlank(),
        ) { Text("join") }
        joinResult?.let { Text(it) }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SendScreen() {
    val ctx = LocalContext.current
    var expanded by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    var cats by remember { mutableStateOf<List<Cat>>(emptyList()) }
    var target by remember { mutableStateOf<String?>(null) }
    var result by remember { mutableStateOf<String?>(null) }

    LaunchedEffect(Unit) {
        val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), "cats") }
        r?.optJSONArray("cats")?.let { arr ->
            cats = (0 until arr.length()).mapNotNull { i ->
                arr.getJSONObject(i).let { o ->
                    if (o.optBoolean("storer", false) || o.optBoolean("dropbox", false)) null
                    else Cat(o.optString("name"), false, false, o.optString("key"))
                }
            }
        }
    }

    val pickFile = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null && target != null) {
            scope.launch {
                result = withContext(Dispatchers.IO) { sendUri(ctx, uri, target!!) }
            }
        }
    }

    Column(
        Modifier
            .fillMaxSize()
            .padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        SectionHeader("send a file")
        if (cats.isEmpty()) {
            Text(
                "no cats to send to yet — pair one from the Pair tab",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        } else {
            ExposedDropdownMenuBox(
                expanded = expanded,
                onExpandedChange = { expanded = it },
            ) {
                OutlinedTextField(
                    value = target ?: "",
                    onValueChange = {},
                    readOnly = true,
                    label = { Text("send to") },
                    trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded) },
                    modifier = Modifier
                        .fillMaxWidth()
                        .menuAnchor(MenuAnchorType.PrimaryNotEditable),
                )
                DropdownMenu(
                    expanded = expanded,
                    onDismissRequest = { expanded = false },
                ) {
                    cats.forEach { c ->
                        DropdownMenuItem(
                            text = { Text(c.name) },
                            onClick = {
                                target = c.name
                                expanded = false
                            },
                        )
                    }
                }
            }
        }
        Button(
            onClick = { pickFile.launch(arrayOf("*/*")) },
            enabled = target != null,
        ) { Text("pick a file for ${target ?: "…"}") }
        result?.let { Text(it) }
    }
}

/** Copies the picked SAF document into the sandbox (the daemon needs a
 * real path, not a content URI) and queues the send. */
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

@Composable
fun InboxScreen() {
    val ctx = LocalContext.current
    var files by remember { mutableStateOf(listOf<File>()) }
    var refreshKey by remember { mutableIntStateOf(0) }

    LaunchedEffect(refreshKey) {
        files = withContext(Dispatchers.IO) {
            ClowdService.inboxDir(ctx).listFiles()?.sortedByDescending { it.lastModified() } ?: emptyList()
        }
    }

    // The daemon lands files in the inbox dir; watch it instead of a
    // manual refresh. (The deprecated String constructor covers
    // minSdk 26; the File one is API 29+.)
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
                .padding(start = 24.dp, end = 24.dp, top = 24.dp, bottom = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text("inbox", style = MaterialTheme.typography.titleMedium)
        }
        if (files.isEmpty()) {
            Text(
                "nothing received yet",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(horizontal = 24.dp),
            )
        }
        LazyColumn {
            items(files) { f ->
                ListItem(
                    headlineContent = { Text(f.name) },
                    supportingContent = {
                        Text(
                            "${humanBytes(f.length())} · " +
                                DateUtils.getRelativeTimeSpanString(f.lastModified()).toString(),
                        )
                    },
                    modifier = Modifier.fillMaxWidth(),
                    trailingContent = {
                        TextButton(onClick = { openFile(ctx, f) }) { Text("open") }
                    },
                )
            }
        }
    }
}

private fun openFile(ctx: android.content.Context, f: File) {
    val uri: Uri = FileProvider.getUriForFile(ctx, "${ctx.packageName}.files", f)
    val intent = Intent(Intent.ACTION_VIEW)
        .setDataAndType(uri, ctx.contentResolver.getType(uri) ?: "application/octet-stream")
        .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    runCatching { ctx.startActivity(Intent.createChooser(intent, f.name)) }
}

@Composable
fun LogScreen() {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant,
        modifier = Modifier.fillMaxSize(),
    ) {
        var log by remember { mutableStateOf(ClowdService.recentLog()) }
        val listState = rememberLazyListState()
        val scope = rememberCoroutineScope()
        // Follow the tail unless the user scrolled away from it; the
        // Follow button pins back to the newest line.
        var follow by remember { mutableStateOf(true) }

        // Poll the service's ring buffer; while following, pin to the
        // newest line.
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

        // A user scroll that leaves the newest line stops the pin.
        LaunchedEffect(listState) {
            snapshotFlow { listState.isScrollInProgress }
                .collect { scrolling ->
                    if (scrolling) {
                        val info = listState.layoutInfo
                        val last = info.visibleItemsInfo.lastOrNull()?.index ?: 0
                        if (info.totalItemsCount > 0 && last < info.totalItemsCount - 1) {
                            follow = false
                        }
                    }
                }
        }

        Box(Modifier.fillMaxSize()) {
            SelectionContainer {
                LazyColumn(state = listState, modifier = Modifier.padding(12.dp)) {
                    items(if (log.isEmpty()) emptyList() else log.split("\n")) { line ->
                        Text(
                            line,
                            fontFamily = FontFamily.Monospace,
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
            }
            if (!follow) {
                ExtendedFloatingActionButton(
                    text = { Text("Follow") },
                    icon = { Icon(Icons.Outlined.KeyboardArrowDown, contentDescription = null) },
                    onClick = {
                        follow = true
                        scope.launch {
                            val n = listState.layoutInfo.totalItemsCount
                            if (n > 0) listState.scrollToItem(n - 1)
                        }
                    },
                    modifier = Modifier
                        .align(Alignment.BottomEnd)
                        .padding(16.dp),
                )
            }
        }
    }
}

/** Runs [block] on Dispatchers.IO, returning null on any failure (a
 * socket error means the daemon is down; the UI treats null as
 * unreachable). */
private suspend fun <T> withContextOrNull(block: () -> T): T? = withContext(Dispatchers.IO) {
    runCatching { block() }.getOrNull()
}
