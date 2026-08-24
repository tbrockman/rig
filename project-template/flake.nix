# Per-project devShell template.
#
# The base image carries the NVIDIA *driver* and nothing else. Toolchains live
# here, in the project, so two projects can disagree about their CUDA version
# without either of them needing a new base image.
#
#   nix develop            # toolchain on PATH
#   nix develop -c make run
{
  description = "GPU project devShell (CUDA)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        # The CUDA redistributables are unfree. This does NOT set
        # config.cudaSupport — that would rebuild half of nixpkgs against CUDA
        # for no benefit here. We only want nvcc and the runtime.
        config.allowUnfree = true;
      };
      cuda = pkgs.cudaPackages;
    in {
      devShells.${system}.default =
        # nvcc rejects host compilers newer than it knows about. backendStdenv
        # is the gcc that this cudaPackages set was built to accept; using the
        # default stdenv is the usual source of "unsupported GNU version".
        (pkgs.mkShell.override { stdenv = cuda.backendStdenv; }) {
          name = "cuda-dev";

          packages = [
            cuda.cuda_nvcc            # nvcc, cicc, ptxas
            cuda.cuda_cudart          # libcudart + cuda_runtime.h
            cuda.cuda_cccl            # thrust / cub / libcu++ headers
            cuda.cuda_nvml_dev        # nvml, for anything that queries the GPU
            cuda.cuda_cuobjdump
            cuda.cuda_nvdisasm
            pkgs.gnumake
          ];

          shellHook = ''
            # libcuda.so.1 belongs to the *driver*, not the toolkit, so it is
            # not in any of the packages above. On NixOS it lives in
            # /run/opengl-driver/lib. Without this, a binary that compiles and
            # links cleanly dies at runtime with
            # "CUDA driver version is insufficient" or cudaErrorInsufficientDriver.
            if [ -d /run/opengl-driver/lib ]; then
              export LD_LIBRARY_PATH=/run/opengl-driver/lib''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
            fi
            export CUDA_PATH=''${CUDA_PATH:-${cuda.cuda_nvcc}}
          '';
        };
    };
}
