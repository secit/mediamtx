BINARY_NAME = mediamtx

define DOCKERFILE_BINARIES
FROM debian:bullseye-slim AS toolchain-arm
RUN dpkg --add-architecture armhf && apt-get update && \
  apt-get install -y --no-install-recommends \
  ca-certificates git cmake ninja-build pkg-config \
  gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf \
  libssl-dev:armhf zlib1g-dev:armhf make file \
  wget
RUN dpkg --add-architecture arm64 && apt-get update && \
	apt-get install -y --no-install-recommends \
	gcc-aarch64-linux-gnu g++-aarch64-linux-gnu \
	libssl-dev:arm64 zlib1g-dev:arm64
WORKDIR /build

RUN git clone --depth=1 https://github.com/Haivision/srt.git
WORKDIR /build/srt/build-arm
RUN CC=arm-linux-gnueabihf-gcc CXX=arm-linux-gnueabihf-g++ \
  cmake .. -G Ninja \
  -DENABLE_SHARED=OFF \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_SYSTEM_NAME=Linux \
  -DCMAKE_SYSTEM_PROCESSOR=arm \
  -DCMAKE_INSTALL_PREFIX=/opt/arm-srt && \
  ninja && ninja install && \
  file /opt/arm-srt/lib/libsrt.a

WORKDIR /build/srt/build-arm64
RUN CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ \
  cmake .. -G Ninja \
  -DENABLE_SHARED=OFF \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_SYSTEM_NAME=Linux \
  -DCMAKE_SYSTEM_PROCESSOR=aarch64 \
  -DCMAKE_INSTALL_PREFIX=/opt/arm64-srt && \
  ninja && ninja install && \
  file /opt/arm64-srt/lib/libsrt.a

FROM golang:1.24-bullseye AS gobuild
WORKDIR /src
COPY . .

RUN rm -rf tmp binaries && \
  mkdir tmp binaries && \
  cp mediamtx.yml LICENSE tmp/ && \
  go generate ./...

FROM gobuild as gobuild-amd64
RUN apt-get update && \
  apt-get install -y --no-install-recommends \
  libssl-dev zlib1g-dev pkg-config file libsrt-openssl-dev && \
  rm -rf /var/lib/apt/lists/*

RUN go build -o "tmp/mediamtx" && \
  tar -C tmp -czf "binaries/$(BINARY_NAME)_$$(cat internal/core/VERSION)_linux_amd64.tar.gz" --owner=0 --group=0 "mediamtx" mediamtx.yml LICENSE

FROM gobuild AS gobuild-armv7
RUN dpkg --add-architecture armhf && apt-get update && \
  apt-get install -y --no-install-recommends \
  gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf \
  libssl-dev:armhf zlib1g-dev:armhf pkg-config file && \
  rm -rf /var/lib/apt/lists/*

COPY --from=toolchain-arm /opt/arm-srt /usr

ENV GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=1 \
  CC=arm-linux-gnueabihf-gcc CXX=arm-linux-gnueabihf-g++ \
  CGO_LDFLAGS="-lsrt -lcrypto -lssl -lz -lpthread -lm -lstdc++" \
  PKG_CONFIG_PATH=/opt/arm-srt/lib/pkgconfig \
  PKG_CONFIG_LIBDIR=/opt/arm-srt/lib/pkgconfig

RUN go build -o "tmp/$(BINARY_NAME)" && \
  tar -C tmp -czf "binaries/$(BINARY_NAME)_$$(cat internal/core/VERSION)_linux_armv7.tar.gz" --owner=0 --group=0 "mediamtx" mediamtx.yml LICENSE

FROM gobuild AS gobuild-arm64
RUN dpkg --add-architecture arm64 && apt-get update && \
  apt-get install -y --no-install-recommends \
	gcc-aarch64-linux-gnu g++-aarch64-linux-gnu \
	libssl-dev:arm64 zlib1g-dev:arm64 pkg-config file && \
  rm -rf /var/lib/apt/lists/*

COPY --from=toolchain-arm /opt/arm64-srt /usr

ENV GOOS=linux GOARCH=arm64 CGO_ENABLED=1 \
  CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ \
  CGO_LDFLAGS="-lsrt -lcrypto -lssl -lz -lpthread -lm -lstdc++" \
  PKG_CONFIG_PATH=/opt/arm64-srt/lib/pkgconfig \
  PKG_CONFIG_LIBDIR=/opt/arm64-srt/lib/pkgconfig

RUN go build -o "tmp/$(BINARY_NAME)" && \
  tar -C tmp -czf "binaries/$(BINARY_NAME)_$$(cat internal/core/VERSION)_linux_arm64.tar.gz" --owner=0 --group=0 "mediamtx" mediamtx.yml LICENSE

FROM debian:bullseye-slim AS final
COPY --from=gobuild-amd64 /src/binaries/*.tar.gz /src/binaries/
COPY --from=gobuild-armv7 /src/binaries/*.tar.gz /src/binaries/
COPY --from=gobuild-arm64 /src/binaries/*.tar.gz /src/binaries/
endef
export DOCKERFILE_BINARIES

haivision:
	echo "$$DOCKERFILE_BINARIES" | docker build . -f - \
	-t temp
	CID=$$(docker create temp) && \
	rm -rf ./binaries/* && \
	docker cp $$CID:/src/binaries/. ./binaries && \
	docker rm $$CID
