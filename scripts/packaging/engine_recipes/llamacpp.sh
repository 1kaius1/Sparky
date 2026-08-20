#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# llama.cpp recipe for scripts/build_engine_release.sh - see that script's own
# header for how recipes are sourced and used, and PLANNING.md's Decisions Log
# for the overall engine-release-tooling design.
#
# This file only defines variables - it does not execute anything on its own,
# and does not set shell options (that's build_engine_release.sh's decision).
# It is the concrete extension point for adding a koboldcpp recipe later:
# adding that engine means adding a sibling file here plus one recipe lookup,
# not touching build_engine_release.sh itself.

# recipe_repo_url is the upstream git repository to clone. Overridable via
# ENGINE_REPO (set before sourcing this file) for local testing against a
# fork or a different remote - the recipe only supplies the real default.
recipe_repo_url="${ENGINE_REPO:-https://github.com/ggml-org/llama.cpp.git}"

# recipe_cmake_flags are llama.cpp-specific CMake configure flags, space-
# separated. CMAKE_CUDA_ARCHITECTURES is deliberately NOT set here - it is
# fleet-hardware policy supplied by build_engine_release.sh's own
# CUDA_ARCHITECTURES parameter, not an engine-source-code fact this recipe
# should own.
#
#   GGML_CUDA=ON       - the current flag name for CUDA GPU offload support.
#                        LLAMA_CUBLAS was the old name; llama.cpp's own CMake
#                        option names have drifted before (LLAMA_CUBLAS ->
#                        GGML_CUDA) and may drift again - re-check this
#                        against the actual target tag's CMakeLists.txt on
#                        every real build, don't assume this stays accurate
#                        forever.
#   BUILD_SHARED_LIBS=ON - produces the lib*.so files docs/AGENT.md's
#                        documented on-disk layout requires alongside
#                        llama-server, not just a static binary.
#   GGML_NATIVE=OFF    - explicitly disables host-CPU auto-tuning. The build
#                        machine's CPU may support different SIMD extensions
#                        than the real fleet's inference hosts; an
#                        auto-tuned "native" build risks SIGILL at runtime on
#                        real hardware that lacks whatever instruction set
#                        the build machine happened to have.
#   LLAMA_CURL=OFF     - this is a server-only build (Sparky's agent already
#                        owns model downloads via agent/transfer), so this
#                        avoids needing libcurl-dev on the build machine for
#                        a feature that's never used through this path.
recipe_cmake_flags="-DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=ON -DBUILD_SHARED_LIBS=ON -DGGML_NATIVE=OFF -DLLAMA_CURL=OFF"

# recipe_primary_binary is the one file build_engine_release.sh treats as
# mandatory - packaging fails loudly if this isn't found anywhere under the
# build tree, rather than silently shipping an incomplete tarball.
recipe_primary_binary="llama-server"

# recipe_lib_glob matches the supporting shared libraries GGML_CUDA=ON +
# BUILD_SHARED_LIBS=ON produce (ggml/llama's own .so files) - packaged
# alongside recipe_primary_binary, but never fails the build if empty, since
# a static-linked future recipe variant might legitimately have none.
recipe_lib_glob="lib*.so*"
