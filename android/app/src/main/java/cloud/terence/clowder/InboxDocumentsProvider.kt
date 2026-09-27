package cloud.terence.clowder

import android.database.MatrixCursor
import android.os.CancellationSignal
import android.os.ParcelFileDescriptor
import android.provider.DocumentsContract.Document
import android.provider.DocumentsContract.Root
import android.provider.DocumentsProvider
import android.webkit.MimeTypeMap
import java.io.File
import java.io.FileNotFoundException

/**
 * Exposes the sandboxed inbox to the system's file manager and any
 * other SAF client. The inbox lives in app-private storage, which no
 * external file manager can reach; a DocumentsProvider makes it a
 * first-class root — the Files app shows "Clowder inbox" in its
 * sidebar, browsable, openable and copyable out, read-only. This is
 * the same mechanism the CLI covers with `clow inbox`.
 */
class InboxDocumentsProvider : DocumentsProvider() {

    companion object {
        const val AUTHORITY = "cloud.terence.clowder.inbox"
        const val ROOT_ID = "inbox"

        // The root document's id; every other id is a path relative
        // to the inbox dir.
        private const val ROOT_DOC_ID = "root"

        private val ROOT_PROJECTION = arrayOf(
            Root.COLUMN_ROOT_ID,
            Root.COLUMN_TITLE,
            Root.COLUMN_ICON,
            Root.COLUMN_FLAGS,
            Root.COLUMN_AVAILABLE_BYTES,
        )
        private val DOCUMENT_PROJECTION = arrayOf(
            Document.COLUMN_DOCUMENT_ID,
            Document.COLUMN_DISPLAY_NAME,
            Document.COLUMN_MIME_TYPE,
            Document.COLUMN_FLAGS,
            Document.COLUMN_SIZE,
            Document.COLUMN_LAST_MODIFIED,
        )
    }

    private val inboxDir: File
        get() = ClowdService.inboxDir(requireNotNull(context))

    override fun onCreate(): Boolean = true

    override fun queryRoots(projection: Array<out String>?): MatrixCursor {
        val cols = projection ?: ROOT_PROJECTION
        val cursor = MatrixCursor(cols)
        val row = cursor.newRow()
        for (col in cols) {
            when (col) {
                Root.COLUMN_ROOT_ID -> row.add(ROOT_ID)
                Root.COLUMN_TITLE -> row.add("Clowder inbox")
                Root.COLUMN_ICON -> row.add(R.mipmap.ic_launcher)
                // Local files only; every flag (search, recent files)
                // stays off — the inbox is tiny.
                Root.COLUMN_FLAGS -> row.add(Root.FLAG_LOCAL_ONLY)
                Root.COLUMN_AVAILABLE_BYTES -> row.add(inboxDir.freeSpace)
            }
        }
        return cursor
    }

    override fun queryDocument(docId: String, projection: Array<out String>?): MatrixCursor {
        val f = fileFor(docId) ?: throw FileNotFoundException(docId)
        val cursor = MatrixCursor(projection ?: DOCUMENT_PROJECTION)
        includeFile(cursor, projection ?: DOCUMENT_PROJECTION, docId, f)
        return cursor
    }

    override fun queryChildDocuments(
        parentDocId: String,
        projection: Array<out String>?,
        sortOrder: String?,
    ): MatrixCursor {
        val cols = projection ?: DOCUMENT_PROJECTION
        val parent = fileFor(parentDocId) ?: throw FileNotFoundException(parentDocId)
        val cursor = MatrixCursor(cols)
        val children = parent.listFiles() ?: return cursor
        for (f in children) {
            val childId = if (parentDocId == ROOT_DOC_ID) f.name else "$parentDocId/${f.name}"
            includeFile(cursor, cols, childId, f)
        }
        return cursor
    }

    override fun openDocument(
        docId: String,
        mode: String,
        signal: CancellationSignal?,
    ): ParcelFileDescriptor {
        val f = fileFor(docId) ?: throw FileNotFoundException(docId)
        // Read-only root-to-leaf: no write flags are advertised, so
        // read is the only mode that arrives anyway.
        return ParcelFileDescriptor.open(f, ParcelFileDescriptor.MODE_READ_ONLY)
    }

    override fun isChildDocument(parentDocId: String, docId: String): Boolean {
        val parent = fileFor(parentDocId) ?: return false
        val child = fileFor(docId) ?: return false
        return child.toPath().startsWith(parent.toPath())
    }

    override fun getDocumentType(docId: String): String {
        val f = fileFor(docId) ?: throw FileNotFoundException(docId)
        return if (f.isDirectory) Document.MIME_TYPE_DIR else mimeFor(f)
    }

    /** One row describing f under docId, in the requested projection. */
    private fun includeFile(cursor: MatrixCursor, cols: Array<out String>, docId: String, f: File) {
        val isDir = f.isDirectory
        val row = cursor.newRow()
        for (col in cols) {
            when (col) {
                Document.COLUMN_DOCUMENT_ID -> row.add(docId)
                Document.COLUMN_DISPLAY_NAME ->
                    row.add(if (docId == ROOT_DOC_ID) "Clowder inbox" else f.name)
                Document.COLUMN_MIME_TYPE ->
                    row.add(if (isDir) Document.MIME_TYPE_DIR else mimeFor(f))
                // Zero: no delete/rename/write. Copying out and
                // opening are handled by the browsing client.
                Document.COLUMN_FLAGS -> row.add(0)
                Document.COLUMN_SIZE -> row.add(if (isDir) 0L else f.length())
                Document.COLUMN_LAST_MODIFIED -> row.add(f.lastModified())
            }
        }
    }

    /** Resolves a document id to a file inside the inbox, refusing
     *  anything that would escape it. */
    private fun fileFor(docId: String): File? {
        val root = inboxDir.canonicalFile
        if (docId == ROOT_DOC_ID) return root
        val f = File(root, docId)
        val canonical = runCatching { f.canonicalFile }.getOrDefault(null) ?: return null
        if (canonical.path != root.path &&
            !canonical.path.startsWith(root.path + File.separator)
        ) {
            return null
        }
        return canonical
    }

    private fun mimeFor(f: File): String {
        val ext = f.extension.lowercase()
        if (ext.isEmpty()) return "application/octet-stream"
        return MimeTypeMap.getSingleton()
            .getMimeTypeFromExtension(ext) ?: "application/octet-stream"
    }
}
