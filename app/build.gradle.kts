import java.util.Properties

plugins {
    id("com.android.application")
}

val keystoreProperties = Properties()
val keystorePropertiesFile = rootProject.file("keystore.properties")
if (keystorePropertiesFile.exists()) {
    keystorePropertiesFile.inputStream().use(keystoreProperties::load)
}

android {
    namespace = "com.wb2api.gateway"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.wb2api.gateway"
        minSdk = 24
        targetSdk = 35
        versionCode = 2
        versionName = "1.9.38"
    }

    signingConfigs {
        create("release") {
            if (keystorePropertiesFile.exists()) {
                storeFile = file(keystoreProperties.getProperty("storeFile"))
                storePassword = keystoreProperties.getProperty("storePassword")
                keyAlias = keystoreProperties.getProperty("keyAlias")
                keyPassword = keystoreProperties.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            if (keystorePropertiesFile.exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    packaging {
        jniLibs {
            // 必须解压：Android 10 起禁止应用执行自己数据目录里的文件，
            // 唯一允许执行的落盘位置就是 nativeLibraryDir。若按 AGP 默认的
            // “不压缩直接 mmap” 打包，nativeLibraryDir 会是空的，Go 可执行文件
            // 就无处可取。useLegacyPackaging = true 等价于 extractNativeLibs="true"。
            useLegacyPackaging = true
            // Go 产物已 -s -w 剥离过，别再让 AGP 的 strip 动它。
            keepDebugSymbols += "**/libwb2api.so"
        }
    }

    lint {
        abortOnError = false
    }
}
