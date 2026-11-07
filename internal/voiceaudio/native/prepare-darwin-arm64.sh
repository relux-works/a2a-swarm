#!/bin/bash
# Preparation only: NOT RUN here; this does not enable the Go platform stub.
# Run solely in a separately admitted offline native-Mac build confinement.
set -euo pipefail
umask 077
test "$(/usr/bin/uname -s)" = Darwin
test "$(/usr/bin/uname -m)" = arm64
: "${VOICEAUDIO_TOOLCHAIN_MANIFEST:?missing admitted Apple SDK/tool manifest}"
: "${VOICEAUDIO_TOOLCHAIN_SHA256:?missing admitted manifest digest}"
printf '%s  %s\n' "$VOICEAUDIO_TOOLCHAIN_SHA256" "$VOICEAUDIO_TOOLCHAIN_MANIFEST" | /usr/bin/shasum -a 256 -c -
/usr/bin/shasum -a 256 -c "$VOICEAUDIO_TOOLCHAIN_MANIFEST"
# Manifest must include these tools, their native closure, SDK headers/stubs,
# /usr/bin/shasum + its Perl closure, make/shell tools and pinned pkg-config.
export PATH=/toolchain/bin:/toolchain/usr/bin:/usr/bin:/bin
export CC=/toolchain/usr/bin/clang AR=/toolchain/usr/bin/ar
export RANLIB=/toolchain/usr/bin/ranlib LD=/toolchain/usr/bin/ld
export CFLAGS='-O2 -fstack-protector-strong -arch arm64 -mmacosx-version-min=12.0 -isysroot /sdk'
export PKG_CONFIG=/toolchain/bin/pkg-config
export PKG_CONFIG_LIBDIR=/build/prefix/lib/pkgconfig
export SOURCE_DATE_EPOCH=1760000000
test -d /sdk
test ! -e /native/voiceaudio-decode
cd /task/data
/usr/bin/shasum -a 256 -c /task/repo/internal/voiceaudio/native/SOURCES.sha256
cd /build
for archive in libogg-1.3.6.tar.xz opus-1.6.1.tar.gz opusfile-0.12.tar.gz opus-tools-0.2.tar.gz; do
  /usr/bin/tar -xf "/task/data/$archive" --no-same-owner --no-same-permissions
done
prefix=/build/prefix
cd /build/libogg-1.3.6
./configure --build=aarch64-apple-darwin --host=aarch64-apple-darwin --prefix="$prefix" --disable-shared --enable-static --disable-dependency-tracking
/usr/bin/make -j2
/usr/bin/make install
cd /build/opus-1.6.1
./configure --build=aarch64-apple-darwin --host=aarch64-apple-darwin --prefix="$prefix" --disable-shared --enable-static --disable-extra-programs --disable-doc --disable-intrinsics --disable-rtcd --disable-dependency-tracking
/usr/bin/make -j2
/usr/bin/make install
cd /build/opusfile-0.12
./configure --build=aarch64-apple-darwin --host=aarch64-apple-darwin --prefix="$prefix" --disable-shared --enable-static --disable-http --disable-examples --disable-doc --disable-dependency-tracking
/usr/bin/make -j2
/usr/bin/make install
common=(-O2 -fstack-protector-strong -arch arm64 -mmacosx-version-min=12.0 -isysroot /sdk -I"$prefix/include/opus" -I"$prefix/include" -I/build/opus-tools-0.2/src -DOUTSIDE_SPEEX -DRANDOM_PREFIX=voiceaudio -DRESAMPLE_FULL_SINC_TABLE -DFLOATING_POINT -DHAVE_LRINTF -DVAR_ARRAYS)
mkdir -p /native/relink
"$CC" "${common[@]}" -c -o /native/relink/decode.o /task/repo/internal/voiceaudio/native/decode.c
"$CC" "${common[@]}" -c -o /native/relink/resample.o /build/opus-tools-0.2/src/resample.c
cp "$prefix/lib/libopusfile.a" "$prefix/lib/libopus.a" "$prefix/lib/libogg.a" /native/relink/
"$CC" "${common[@]}" -o /native/voiceaudio-decode /native/relink/decode.o /native/relink/resample.o /native/relink/libopusfile.a /native/relink/libopus.a /native/relink/libogg.a -lm
/toolchain/usr/bin/otool -hv /native/voiceaudio-decode
/toolchain/usr/bin/otool -l /native/voiceaudio-decode
/toolchain/usr/bin/otool -L /native/voiceaudio-decode
/usr/bin/shasum -a 256 /native/voiceaudio-decode
# STOP: no execution, signing or installation. Native resource enforcement,
# loader allowlist + OS closure, artifact pin and adapter admission remain open.
