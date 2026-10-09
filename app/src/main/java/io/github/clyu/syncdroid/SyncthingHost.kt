package io.github.clyu.syncdroid

import android.content.Context
import android.content.Intent
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.util.Base64
import io.github.clyu.syncdroid.bridge.Bridge
import java.io.File
import java.security.SecureRandom
import kotlin.concurrent.thread
import kotlin.system.exitProcess

/**
 * Runs Syncthing in this process. A process can start Syncthing only once, and Syncthing outlives
 * any one activity, hence a singleton. Apart from the thread that [start] spawns, everything here
 * happens on the main thread.
 */
object SyncthingHost {
    sealed interface State {
        data object Starting : State
        data class Running(val guiUrl: String) : State
        data class Failed(val message: String) : State
    }

    private const val GUI_USER = "syncdroid"

    /** The exit status with which Syncthing asks to be started again. */
    private const val EXIT_RESTART = 3L

    // Any app on the device can reach the GUI on loopback, and the GUI can sync every file this
    // app has access to. The password is new for every process and never leaves it.
    private val guiPassword =
        ByteArray(32).also { SecureRandom().nextBytes(it) }.joinToString("") { "%02x".format(it) }

    /** The Authorization header that the GUI lets in. */
    val guiAuthorization: String =
        "Basic " + Base64.encodeToString("$GUI_USER:$guiPassword".toByteArray(), Base64.NO_WRAP)

    /** Null until [start] is called. */
    var state: State? = null
        private set

    /** The activity there is, if any: it shows [state], and is what a restart brings back. */
    var activity: MainActivity? = null

    private val mainThread = Handler(Looper.getMainLooper())

    fun start(context: Context) {
        if (state != null) return
        setState(State.Starting)

        val app = context.applicationContext
        thread(name = "syncthing") {
            val started = try {
                State.Running(
                    Bridge.start(
                        File(app.filesDir, "syncthing").path,
                        Environment.getExternalStorageDirectory().path,
                        app.cacheDir.path,
                        GUI_USER,
                        guiPassword,
                    )
                )
            } catch (e: Exception) {
                State.Failed(e.message ?: e.toString())
            }
            mainThread.post { setState(started) }

            if (started is State.Running) {
                // Returns when the user shuts Syncthing down or restarts it from the GUI.
                val status = Bridge.waitForExit()
                mainThread.post { exit(restart = status == EXIT_RESTART) }
            }
        }
    }

    private fun setState(newState: State) {
        state = newState
        activity?.show(newState)
    }

    /** Syncthing has stopped and cannot run in this process again, so the process ends too. */
    private fun exit(restart: Boolean) {
        activity?.let {
            if (restart) {
                // The system starts a new process for the activity once this one is gone.
                it.startActivity(Intent.makeRestartActivityTask(it.componentName))
            } else {
                it.finishAndRemoveTask()
            }
        }
        exitProcess(0)
    }
}
