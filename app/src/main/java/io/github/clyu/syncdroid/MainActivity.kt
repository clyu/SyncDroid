package io.github.clyu.syncdroid

import android.annotation.SuppressLint
import android.app.Activity
import android.app.AlertDialog
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.net.wifi.WifiManager
import android.os.Bundle
import android.os.Environment
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.ViewGroup.LayoutParams.MATCH_PARENT
import android.view.WindowInsets
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.widget.TextView

/** Shows the Syncthing GUI in a WebView, after asking for access to the files it is to sync. */
class MainActivity : Activity() {
    private lateinit var webView: WebView
    private lateinit var status: TextView

    private var storageAccessDialog: AlertDialog? = null

    /** Whether the user chose to go on without access to all files. */
    private var storageAccessSkipped = false

    private var multicastLock: WifiManager.MulticastLock? = null

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        webView = WebView(this).apply {
            visibility = View.GONE
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            webViewClient = GuiClient()
        }
        status = TextView(this).apply {
            gravity = Gravity.CENTER
            val padding = (24 * resources.displayMetrics.density).toInt()
            setPadding(padding, padding, padding, padding)
        }
        val root = FrameLayout(this).apply {
            addView(webView, MATCH_PARENT, MATCH_PARENT)
            addView(status, MATCH_PARENT, MATCH_PARENT)
        }
        setContentView(root)

        // Android 15 and later draw every app edge to edge; do the same on earlier versions, and
        // keep the content clear of the system bars and the keyboard.
        window.setDecorFitsSystemWindows(false)
        root.setOnApplyWindowInsetsListener { view, insets ->
            val obscured = insets.getInsets(
                WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout() or WindowInsets.Type.ime()
            )
            view.setPadding(obscured.left, obscured.top, obscured.right, obscured.bottom)
            WindowInsets.CONSUMED
        }

        SyncthingHost.activity = this
        SyncthingHost.state?.let(::show)
    }

    override fun onStart() {
        super.onStart()
        // Wi-Fi drops the broadcast and multicast packets that local discovery listens for unless
        // an app asks for them, which costs battery, so only ask while on screen.
        multicastLock = applicationContext.getSystemService(WifiManager::class.java)
            ?.createMulticastLock("syncthing")
            ?.apply { acquire() }
    }

    override fun onResume() {
        super.onResume()
        // Also where the user comes back to from the system's settings.
        if (SyncthingHost.state != null) return
        if (Environment.isExternalStorageManager() || storageAccessSkipped) {
            SyncthingHost.start(this)
        } else if (storageAccessDialog == null) {
            askForStorageAccess()
        }
    }

    override fun onStop() {
        multicastLock?.release()
        multicastLock = null
        super.onStop()
    }

    override fun onDestroy() {
        if (SyncthingHost.activity === this) SyncthingHost.activity = null
        storageAccessDialog?.dismiss()
        (webView.parent as ViewGroup).removeView(webView)
        webView.destroy()
        super.onDestroy()
    }

    fun show(state: SyncthingHost.State) {
        when (state) {
            SyncthingHost.State.Starting -> status.setText(R.string.starting)
            is SyncthingHost.State.Failed -> status.text = getString(R.string.start_failed, state.message)
            is SyncthingHost.State.Running -> {
                status.visibility = View.GONE
                webView.visibility = View.VISIBLE
                // The header logs this request in, and the session cookie that Syncthing answers
                // with covers all the requests that follow.
                webView.loadUrl(state.guiUrl, mapOf("Authorization" to SyncthingHost.guiAuthorization))
            }
        }
    }

    private fun askForStorageAccess() {
        storageAccessDialog = AlertDialog.Builder(this)
            .setTitle(R.string.storage_access_title)
            .setMessage(R.string.storage_access_message)
            .setCancelable(false)
            .setPositiveButton(R.string.storage_access_grant) { _, _ -> openStorageAccessSettings() }
            .setNegativeButton(R.string.storage_access_skip) { _, _ ->
                storageAccessSkipped = true
                SyncthingHost.start(this)
            }
            .setOnDismissListener { storageAccessDialog = null }
            .show()
    }

    private fun openStorageAccessSettings() {
        try {
            startActivity(
                Intent(
                    Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION,
                    Uri.fromParts("package", packageName, null),
                )
            )
        } catch (e: ActivityNotFoundException) {
            // Not every device has the page for a single app; all of them have the list of apps.
            startActivity(Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION))
        }
    }

    /** Keeps the WebView on the GUI: links that lead elsewhere open in the browser. */
    private inner class GuiClient : WebViewClient() {
        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
            val gui = (SyncthingHost.state as? SyncthingHost.State.Running)?.guiUrl
            if (gui != null && request.url.authority == Uri.parse(gui).authority) return false
            try {
                startActivity(Intent(Intent.ACTION_VIEW, request.url))
            } catch (e: ActivityNotFoundException) {
                // Nothing on the device opens this kind of link.
            }
            return true
        }
    }
}
