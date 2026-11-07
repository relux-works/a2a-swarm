#!/bin/bash
set -euo pipefail
umask 077
# This is a provisioning recipe, never invoked by Decode. It runs only in the
# documented offline namespace with a caller-admitted toolchain manifest.
: "${VOICEAUDIO_TOOLCHAIN_MANIFEST:?explicit toolchain manifest required}"
: "${VOICEAUDIO_TOOLCHAIN_SHA256:?explicit manifest digest required}"
printf '%s  %s\n' "$VOICEAUDIO_TOOLCHAIN_SHA256" "$VOICEAUDIO_TOOLCHAIN_MANIFEST" | /usr/bin/sha256sum -c -
/usr/bin/sha256sum -c "$VOICEAUDIO_TOOLCHAIN_MANIFEST"
test "$(/go/bin/go env GOVERSION)" = go1.25.5
test ! -e /native/voiceaudio-decode
export CC=/usr/bin/gcc AR=/usr/bin/ar RANLIB=/usr/bin/ranlib
export CFLAGS='-O2 -fstack-protector-strong -ffile-prefix-map=/build=BUILD'
export PKG_CONFIG_LIBDIR=/build/prefix/lib/pkgconfig
export SOURCE_DATE_EPOCH=1760000000
cd /task/data
sha256sum -c /task/repo/internal/voiceaudio/native/SOURCES.sha256
cd /build
for archive in libogg-1.3.6.tar.xz opus-1.6.1.tar.gz opusfile-0.12.tar.gz opus-tools-0.2.tar.gz; do
  tar --extract --file="/task/data/$archive" --no-same-owner --no-same-permissions
done
prefix=/build/prefix
cd /build/libogg-1.3.6
./configure --prefix="$prefix" --disable-shared --enable-static --disable-dependency-tracking
make -j2
make install
cd /build/opus-1.6.1
./configure --prefix="$prefix" --disable-shared --enable-static --disable-extra-programs --disable-doc --disable-intrinsics --disable-rtcd --disable-dependency-tracking
make -j2
make install
cd /build/opusfile-0.12
./configure --prefix="$prefix" --disable-shared --enable-static --disable-http --disable-examples --disable-doc --disable-dependency-tracking
make -j2
make install
cd /build
common=(-O2 -fstack-protector-strong -ffile-prefix-map=/build=BUILD -I"$prefix/include/opus" -I"$prefix/include" -I/build/opus-tools-0.2/src -DOUTSIDE_SPEEX -DRANDOM_PREFIX=voiceaudio -DRESAMPLE_FULL_SINC_TABLE -DFLOATING_POINT -DHAVE_LRINTF -DVAR_ARRAYS)
mkdir -p /native/relink
gcc "${common[@]}" -c -o /native/relink/decode.o /task/repo/internal/voiceaudio/native/decode.c
gcc "${common[@]}" -c -o /native/relink/resample.o /build/opus-tools-0.2/src/resample.c
cp "$prefix/lib/libopusfile.a" "$prefix/lib/libopus.a" "$prefix/lib/libogg.a" /native/relink/
gcc -static -Wl,--build-id=none -o /native/voiceaudio-decode /native/relink/decode.o /native/relink/resample.o /native/relink/libopusfile.a /native/relink/libopus.a /native/relink/libogg.a -lm
gcc -O2 -static -Wl,--build-id=none -I"$prefix/include/opus" -I"$prefix/include" -o /native/synthetic-encode /task/repo/internal/voiceaudio/native/synthetic_encode.c "$prefix/lib/libopus.a" "$prefix/lib/libogg.a" -lm
readelf -h -l -d /native/voiceaudio-decode
sha256sum /native/voiceaudio-decode /native/synthetic-encode
gcc --version
ld --version
go version
