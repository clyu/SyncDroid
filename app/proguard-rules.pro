# The Go library looks these classes and their members up by name through JNI. The AAR that
# gomobile builds brings rules to the same effect; these are here so that the app does not rely
# on that.
-keep class go.** { *; }
-keep class io.github.clyu.syncdroid.bridge.** { *; }
