#!/bin/bash
set -euo pipefail
mobile_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$mobile_dir"
case "${1:-}" in
  android|ios) platform="$1" ;;
  android-release) platform=android ;;
  ios-ipa|ios-ipa-basic|ios-testflight)
    platform=ios
    if [[ $# != 2 || ! $2 =~ ^[A-Z0-9]{10}$ ]]; then
      echo "Usage: mobile/build.sh $1 TEAM_ID (10 uppercase letters or digits)" >&2
      exit 2
    fi
    if [[ "$1" == ios-testflight ]]; then
      # Cloud signing and upload with an App Store Connect API key of the Admin
      # role; an App Manager key fails with a cloud signing permission error.
      : "${CAGE_ASC_KEY_PATH:?Set the App Store Connect API private key (.p8) path}"
      : "${CAGE_ASC_KEY_ID:?Set the App Store Connect API key ID}"
      : "${CAGE_ASC_ISSUER_ID:?Set the App Store Connect API issuer ID}"
      : "${CAGE_IOS_APP_ID:?Set the bundle identifier registered for this team}"
      : "${CAGE_IOS_BUILD_NUMBER:?Set a build number greater than the last upload of this version}"
      [[ -s "$CAGE_ASC_KEY_PATH" ]] || { echo 'App Store Connect API key is unavailable.' >&2; exit 1; }
      [[ "$CAGE_IOS_BUILD_NUMBER" =~ ^[1-9][0-9]{0,8}$ ]] || { echo 'Invalid iOS build number.' >&2; exit 2; }
    fi
    ;;
  *) echo "Usage: mobile/build.sh android | android-release | ios | ios-ipa TEAM_ID | ios-ipa-basic TEAM_ID | ios-testflight TEAM_ID" >&2; exit 2 ;;
esac
if [[ "$platform" == ios ]]; then
  # xcrun distinguishes a missing SDK from a pending Xcode license or setup step.
  xcrun --sdk iphoneos --show-sdk-path >/dev/null
fi
mkdir -p .build/bin ".build/$platform"
node build-ui.mjs
if [[ "$platform" == android ]]; then bash android/fetch-webkit.sh "$mobile_dir/.build/android"; fi
bash scripts/licenses.sh
(
  cd bindings
  go build -o ../.build/bin/gomobile golang.org/x/mobile/cmd/gomobile
  go build -o ../.build/bin/gobind golang.org/x/mobile/cmd/gobind
)
export PATH="$mobile_dir/.build/bin:$PATH"
if [[ -n "${JAVA_HOME:-}" ]]; then export PATH="$JAVA_HOME/bin:$PATH"; fi
if [[ "$platform" == ios ]]; then
  brand=".build/ios/Assets.xcassets/CageMark.imageset"
  mkdir -p "$brand"
  cp ../ui/src/logo.png "$brand/mark.png"
  printf '%s\n' '{"images":[{"filename":"mark.png","idiom":"universal"}],"info":{"author":"xcode","version":1}}' > "$brand/Contents.json"
  devices=".build/ios/Assets.xcassets/PairDevices.imageset"
  mkdir -p "$devices"
  cp assets/pair-devices.png "$devices/devices.png"
  sed 's/mark.png/devices.png/' "$brand/Contents.json" > "$devices/Contents.json"
  icon=".build/ios/Assets.xcassets/AppIcon.appiconset"
  mkdir -p "$icon"
  printf '%s\n' '{"info":{"author":"xcode","version":1}}' > ".build/ios/Assets.xcassets/Contents.json"
  cp ios/AppIcon.json "$icon/Contents.json"
  sips -z 1024 1024 ../ui/src/logo.png --out "$icon/Cage.png" >/dev/null
  # One arm64 iPhone slice and one simulator slice for this Mac.
  # Each platform framework is then passed to -create-xcframework exactly once.
  (cd bindings && gomobile bind -target="ios/arm64,iossimulator/$(go env GOHOSTARCH)" -iosversion=17.0 -o "$mobile_dir/.build/ios/Cagemobile.xcframework" .)
  if [[ "$1" == ios-testflight ]]; then
    # App Store versions are numeric only: 0.1.8-rc uploads as 0.1.8.
    version="$(tr -d '\r\n' < ../VERSION)"
    marketing="${version%%-*}"
    [[ "$marketing" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid application VERSION.' >&2; exit 2; }
    output=$(mktemp -d "$mobile_dir/.build/ios/testflight-XXXXXXXX")
    # A generated Info.plist keeps the tracked one unchanged. The app uses only
    # exempt standard encryption (TLS, system key storage).
    plist="$output/Info.plist"
    cp ios/CageMobile/Info.plist "$plist"
    plutil -replace CFBundleShortVersionString -string "$marketing" "$plist"
    plutil -replace CFBundleVersion -string "$CAGE_IOS_BUILD_NUMBER" "$plist"
    plutil -replace ITSAppUsesNonExemptEncryption -bool NO "$plist"
    cat > "$output/ExportOptions.plist" <<EXPORT
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>method</key><string>app-store-connect</string>
  <key>destination</key><string>upload</string>
  <key>teamID</key><string>$2</string>
  <key>signingStyle</key><string>automatic</string>
  <key>uploadSymbols</key><true/>
  <key>manageAppVersionAndBuildNumber</key><false/>
</dict>
</plist>
EXPORT
    auth=(-allowProvisioningUpdates -authenticationKeyPath "$CAGE_ASC_KEY_PATH"
      -authenticationKeyID "$CAGE_ASC_KEY_ID" -authenticationKeyIssuerID "$CAGE_ASC_ISSUER_ID")
    xcodebuild -project ios/CageMobile.xcodeproj -scheme CageMobile -configuration Release \
      -destination 'generic/platform=iOS' -archivePath "$output/CageMobile.xcarchive" \
      -derivedDataPath "$output/derived" "${auth[@]}" \
      DEVELOPMENT_TEAM="$2" CAGE_APP_ID="$CAGE_IOS_APP_ID" INFOPLIST_FILE="$plist" archive
    log="$output/export.log"
    xcodebuild -exportArchive -archivePath "$output/CageMobile.xcarchive" \
      -exportOptionsPlist "$output/ExportOptions.plist" -exportPath "$output/export" "${auth[@]}" 2>&1 | tee "$log"
    grep -q 'Upload succeeded' "$log" || { echo 'Export finished without a confirmed upload.' >&2; exit 1; }
    printf 'Cage iPhone %s (%s) uploaded to App Store Connect as %s\n' "$marketing" "$CAGE_IOS_BUILD_NUMBER" "$CAGE_IOS_APP_ID"
  elif [[ "$1" == ios-ipa* ]]; then
    scheme=CageMobile
    if [[ "$1" == ios-ipa-basic ]]; then scheme=CageMobileBasic; fi
    output=$(mktemp -d "$mobile_dir/.build/ios/device-XXXXXXXX")
    xcodebuild -project ios/CageMobile.xcodeproj -scheme "$scheme" -configuration Release \
      -destination 'generic/platform=iOS' -archivePath "$output/CageMobile.xcarchive" \
      DEVELOPMENT_TEAM="$2" archive
    xcodebuild -exportArchive -archivePath "$output/CageMobile.xcarchive" \
      -exportOptionsPlist ios/DebuggingExport.plist -exportPath "$output/export"
    ipa="$output/export/$scheme.ipa"
    # Inspect the exported payload, which Xcode can re-sign after archiving.
    ditto -x -k "$ipa" "$output/check"
    app="$output/check/Payload/$scheme.app"
    test -s "$app/embedded.mobileprovision"
    codesign --verify --deep --strict "$app"
    rm -r "$output/check"
    printf 'Cage iPhone development IPA: %s\n' "$ipa"
  else
    xcodebuild -project ios/CageMobile.xcodeproj -target CageMobile -sdk iphoneos -configuration Debug \
      CODE_SIGNING_ALLOWED=NO SYMROOT="$mobile_dir/.build/ios/products" OBJROOT="$mobile_dir/.build/ios/obj" build
  fi
else
  : "${ANDROID_HOME:?Set ANDROID_HOME to the Android SDK directory}"
  (cd bindings && gomobile bind -target=android/arm64 -androidapi=26 \
    -ldflags='-extldflags=-Wl,-z,max-page-size=16384,-z,common-page-size=16384' \
    -o "$mobile_dir/.build/android/Cagemobile.aar" .)
  if [[ "$1" == android-release ]]; then
    bash android/package.sh --release
  else
    bash android/package.sh
  fi
fi
