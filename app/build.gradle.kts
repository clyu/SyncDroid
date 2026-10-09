plugins {
    alias(libs.plugins.android.application)
}

android {
    namespace = "io.github.clyu.syncdroid"
    compileSdk = 37

    defaultConfig {
        applicationId = "io.github.clyu.syncdroid"
        minSdk = 30
        targetSdk = 36
        // CI numbers its builds, so a later one always installs over an earlier one; local builds are 1.
        versionCode = System.getenv("GITHUB_RUN_NUMBER")?.toInt() ?: 1
        // The Syncthing version inside, then the CI run number so that every build can be told apart.
        versionName = "${(System.getenv("SYNCTHING_VERSION") ?: "v0.0.0").removePrefix("v")}." +
            (System.getenv("GITHUB_RUN_NUMBER") ?: "0")

        // The only ABI the Go library is built for.
        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    signingConfigs {
        // CI signs with a fixed key from repository secrets so every build can update the app
        // already installed on the device.
        System.getenv("SIGNING_KEYSTORE_PATH")?.let { path ->
            create("release") {
                storeFile = file(path)
                storePassword = System.getenv("SIGNING_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("SIGNING_KEY_ALIAS")
                keyPassword = System.getenv("SIGNING_KEYSTORE_PASSWORD")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Without the CI key, fall back to the local debug key so the APK can still be installed.
            signingConfig = signingConfigs.findByName("release") ?: signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    // Syncthing and its Java binding, built from ../bridge with `gomobile bind`; see
    // .github/workflows/android.yml.
    implementation(files("libs/bridge.aar"))
}
