# UNIVERSAL OCUDU gNB — ONE image for EVERY radio: UHD (n310/b210/x310) + DPDK 7.2 O-RU (LiteOn/OFH)
# + MKL (perf) + ZMQ (sim). Built from source, self-contained (no host mounts). The detect init picks the
# path at runtime from the intent `device:` / `split:`.
FROM ubuntu:22.04 AS build
ENV DEBIAN_FRONTEND=noninteractive
RUN echo 'Acquire::Check-Valid-Until "false"; Acquire::Check-Date "false";' > /etc/apt/apt.conf.d/99nc; apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates gnupg wget cmake make g++ pkg-config patch \
      meson ninja-build python3-pyelftools libnuma-dev \
      libboost-all-dev libfftw3-dev libmbedtls-dev libsctp-dev libyaml-cpp-dev libzmq3-dev \
      libusb-1.0-0-dev libudev-dev python3 python3-mako python3-numpy python3-requests \
 && rm -rf /var/lib/apt/lists/*
# Intel oneAPI MKL (redistributable) for FFT/BLAS performance
RUN wget -qO- https://apt.repos.intel.com/intel-gpg-keys/GPG-PUB-KEY-INTEL-SW-PRODUCTS.PUB | gpg --dearmor > /usr/share/keyrings/oneapi.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/oneapi.gpg] https://apt.repos.intel.com/oneapi all main" > /etc/apt/sources.list.d/oneAPI.list \
 && echo 'Acquire::Check-Valid-Until "false"; Acquire::Check-Date "false";' > /etc/apt/apt.conf.d/99nc; apt-get update && apt-get install -y --no-install-recommends intel-oneapi-mkl-devel && rm -rf /var/lib/apt/lists/*
ENV MKLROOT=/opt/intel/oneapi/mkl/latest
# DPDK 23.11 LTS from source (>= OCUDU's 22.11 minimum; Ubuntu apt is only 21.11) — enables 7.2 OFH
RUN git clone --depth 1 --branch v23.11 https://github.com/DPDK/dpdk.git /dpdk \
 && meson setup /dpdk/build /dpdk -Dprefix=/usr/local -Dtests=false -Denable_docs=false -Ddisable_apps='*' \
 && ninja -C /dpdk/build install && ldconfig
# UHD 4.8 from source (N3xx/B2xx)
RUN git clone --depth 1 --branch v4.8.0.0 https://github.com/EttusResearch/uhd.git /uhd \
 && cmake -S /uhd/host -B /uhd/host/build -DENABLE_TESTS=OFF -DENABLE_MANUAL=OFF -DENABLE_PYTHON_API=OFF -DENABLE_C_API=ON -DCMAKE_BUILD_TYPE=Release \
 && cmake --build /uhd/host/build -j"$(nproc)" && cmake --install /uhd/host/build && ldconfig
# Download the SDR firmware/FPGA images. A USB B2xx (b210) LOADS usrp_b200_fw.hex + usrp_b210_fpga.bin from
# HERE at runtime (unlike a networked N3xx, which carries its own onboard firmware). This MUST NOT be a
# best-effort `|| true`: a silently-empty images dir ships a gNB that can't open a b210 ("Could not find
# usrp_b200_fw.hex"). Fail the build loudly if the essential b2xx images are missing.
RUN (uhd_images_downloader -t "b2xx|n3xx" || python3 /usr/local/lib/uhd/utils/uhd_images_downloader.py -t "b2xx|n3xx") \
 && test -f /usr/local/share/uhd/images/usrp_b200_fw.hex \
 && test -f /usr/local/share/uhd/images/usrp_b210_fpga.bin
COPY ocudu-src /src
RUN PKG_CONFIG_PATH=/usr/local/lib/x86_64-linux-gnu/pkgconfig cmake -S /src -B /src/build \
      -DENABLE_UHD=On -DENABLE_DPDK=On -DENABLE_ZEROMQ=On -DENABLE_MKL=On -DMKL_ROOT=/opt/intel/oneapi/mkl/latest \
      -DBUILD_TESTING=False -DCMAKE_BUILD_TYPE=Release -DMARCH=x86-64-v3 \
 && cmake --build /src/build -j"$(nproc)" --target gnb && strip /src/build/apps/gnb/gnb

FROM ubuntu:22.04
ENV DEBIAN_FRONTEND=noninteractive UHD_IMAGES_DIR=/usr/local/share/uhd/images MKL_THREADING_LAYER=GNU
RUN echo 'Acquire::Check-Valid-Until "false"; Acquire::Check-Date "false";' > /etc/apt/apt.conf.d/99nc; apt-get update && apt-get install -y --no-install-recommends \
      libboost-program-options1.74.0 libboost-thread1.74.0 libboost-chrono1.74.0 libboost-date-time1.74.0 \
      libboost-filesystem1.74.0 libboost-serialization1.74.0 libboost-system1.74.0 \
      libfftw3-single3 libfftw3-double3 libsctp1 libzmq5 libyaml-cpp0.7 libmbedtls14 libgomp1 libnuma1 \
      libusb-1.0-0 libudev1 iproute2 iputils-ping ethtool usbutils pciutils ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/lib /usr/local/lib
COPY --from=build /usr/local/share/uhd/images /usr/local/share/uhd/images
COPY --from=build /usr/local/bin/uhd_find_devices /usr/local/bin/uhd_usrp_probe /usr/local/bin/
COPY --from=build /opt/intel/oneapi/mkl/latest/lib /opt/intel/mkl/lib
COPY --from=build /src/build/apps/gnb/gnb /usr/local/bin/gnb
RUN printf '/opt/intel/mkl/lib\n/usr/local/lib\n/usr/local/lib/x86_64-linux-gnu\n' > /etc/ld.so.conf.d/ocudu.conf && ldconfig
ENTRYPOINT ["/usr/local/bin/gnb"]
