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
        ];
      };
    };
}
