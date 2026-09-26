package cloud.terence.clowder

import android.content.Intent
import android.net.Uri
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.FileProvider
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent { ClowderApp() }
    }
}

@Composable
fun ClowderApp() {
    val ctx = LocalContext.current
    var initialized by remember { mutableStateOf(ClowdService.isInitialized(ctx)) }

    if (!initialized) {
        InitScreen { initialized = true }
        return
    }

    var tab by remember { mutableStateOf(0) }
    Scaffold(
        bottomBar = {
            NavigationBar {
                listOf("status", "pair", "send", "inbox", "log").forEachIndexed { i, label ->
                    NavigationBarItem(
                        selected = tab == i,
                        onClick = { tab = i },
                        label = { Text(label) },
                        icon = {},
                    )
                }
            }
        },
    ) { padding ->
        Box(Modifier.padding(padding)) {
            when (tab) {
                0 -> StatusScreen()
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
    val proc = ProcessBuilder(ClowdService.binaryFile(ctx).absolutePath, "init", name)
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

    Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("clowder", style = MaterialTheme.typography.headlineLarge)
        Text("Name this cat. Names are the human handle inside a clowder.")
        OutlinedTextField(value = name, onValueChange = { name = it }, label = { Text("cat name") })
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

@Composable
fun StatusScreen() {
    val ctx = LocalContext.current
    var status by remember { mutableStateOf<Status?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

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

    LazyColumn(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        item {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                if (ClowdService.running) {
                    Button(onClick = { ClowdService.stop(ctx) }) { Text("stop daemon") }
                } else {
                    Button(onClick = { ClowdService.start(ctx) }) { Text("start daemon") }
                }
            }
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        }
        val st = status
        if (st == null || !st.ok) return@LazyColumn

        item { Text("me: ${st.me?.name ?: "?"}", style = MaterialTheme.typography.titleMedium) }

        item { Text("clowder: ${st.cats.size} cats") }
        items(st.cats) { c ->
            val seen = st.liveness[c.key]?.takeIf { it != 0L }
            val path = st.paths[c.key]
            val route = when {
                path == null -> ""
                path.direct -> "direct ${path.endpoint}"
                else -> "relayed"
            }
            Text(
                "  ${c.name}${if (c.dropbox) " [dropbox]" else if (c.storer) " [storer]" else ""}  " +
                    livenessText(seen) + (if (route.isEmpty()) "" else "  $route"),
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
            )
        }

        if (st.transfers.isNotEmpty()) {
            item { Text("transfers") }
            items(st.transfers) { t ->
                Text(
                    "  ${if (t.receiving) "receiving" else "sending"} ${t.fileName} ${t.peer} " +
                        "${100 * t.done / t.total.coerceAtLeast(1)}%",
                    fontFamily = FontFamily.Monospace,
                    fontSize = 13.sp,
                )
            }
        }

        if (st.outbox.isNotEmpty()) {
            item { Text("outbox: ${st.outbox.size} pending") }
            items(st.outbox) { e -> Text("  ${e.fileName} -> ${e.targetName}", fontFamily = FontFamily.Monospace, fontSize = 13.sp) }
        }

        item {
            Text(
                "stats: sent ${st.sentFiles} (${humanBytes(st.sentBytes)}), " +
                    "received ${st.receivedFiles} (${humanBytes(st.receivedBytes)})",
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
            )
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

    Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Button(onClick = {
            scope.launch {
                val r = withContextOrNull { ipc(ClowdService.socketFile(ctx), "invite") }
                inviteMessage = if (r != null && r.optBoolean("ok", false)) {
                    // "ask the other cat to run: clow join <code>" — show the code.
                    r.optString("message").removePrefix("ask the other cat to run: clow join ")
                } else {
                    r?.optString("error") ?: "daemon not reachable"
                }
            }
        }) { Text("invite (new code, valid 5m)") }

        inviteMessage?.let { code ->
            Text(code, fontFamily = FontFamily.Monospace)
            TextButton(onClick = { clipboard.setText(AnnotatedString(code)) }) { Text("copy code") }
        }

        HorizontalDivider()
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

@Composable
fun SendScreen() {
    val ctx = LocalContext.current
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

    Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("send a file", style = MaterialTheme.typography.titleMedium)
        cats.forEach { c ->
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                RadioButton(selected = target == c.name, onClick = { target = c.name })
                Text(c.name)
            }
        }
        Button(onClick = { pickFile.launch(arrayOf("*/*")) }, enabled = target != null) {
            Text("pick file for ${target ?: "?"}")
        }
        result?.let { Text(it) }
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

    Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text("inbox", style = MaterialTheme.typography.titleMedium)
        Button(onClick = { refreshKey++ }) { Text("refresh") }
        LazyColumn {
            items(files) { f ->
                TextButton(onClick = { openFile(ctx, f) }) {
                    Column {
                        Text(f.name)
                        Text("${humanBytes(f.length())}  ${f.lastModified() / 1000}s", fontSize = 12.sp)
                    }
                }
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
    Text(
        ClowdService.recentLog(),
        fontFamily = FontFamily.Monospace,
        fontSize = 11.sp,
        modifier = Modifier
            .padding(12.dp)
            .fillMaxSize(),
    )
}

/** Runs [block] on Dispatchers.IO, returning null on any failure (a
 *  socket error means the daemon is down; the UI treats null as
 *  unreachable). */
private suspend fun <T> withContextOrNull(block: () -> T): T? = withContext(Dispatchers.IO) {
    runCatching { block() }.getOrNull()
}
