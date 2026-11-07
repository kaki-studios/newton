{
  description = "planck.js project";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
    in
    {
      devShells.${system}.default = nixpkgs.legacyPackages.${system}.mkShell {
        packages = [
          nixpkgs.legacyPackages.${system}.nodejs_24
          nixpkgs.legacyPackages.${system}.prettier
          nixpkgs.legacyPackages.${system}.typescript_5
          # The backend. Go 1.26 is required, not 1.24: echo v4.16.3, modernc.org/sqlite
          # v1.61.1 and goose v3.29.0 all declare `go 1.26.0`.
          nixpkgs.legacyPackages.${system}.go_1_26
        ];
        shellHook = ''
          # The release build is CGO_ENABLED=0, but `go test -race` needs cgo. NixOS has no
          # /usr/bin/gcc, so point the Go toolchain at the clang in this shell instead of failing
          # with "cgo: C compiler gcc not found".
          export CC=clang
          echo "newton dev shell: node $(node --version), go $(go version | cut -d' ' -f3)"
          echo "  npm test        TypeScript"
          echo "  go test ./...   Go (in server/)"
          echo "  npm run dev:full  backend + frontend, fake model provider"
        '';
      };

      # A reproducible build of the server, so `nix build` and the Dockerfile agree about what the
      # binary is. The static payload comes from npm; the image is still the deployment artifact.
      packages.${system}.default = nixpkgs.legacyPackages.${system}.buildGoModule {
        pname = "newton";
        version = "0.1.0";
        src = ./server;
        # Populate with `nix build` on first use; the Dockerfile builds without a fixed hash.
        vendorHash = null;
        subPackages = [ "cmd/api" ];
        CGO_ENABLED = 0;
        ldflags = [
          "-s"
          "-w"
          "-X main.version=0.1.0"
        ];
      };
    };
}
