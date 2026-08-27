{
  description = "Stuff: a durable store for Items and Notes";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.buildGoModule {
            pname = "stuff";
            version = "0.1.0";
            src = self;
            vendorHash = "sha256-qVoj03LNLbdoCUAOydK7oEHsuZ1BZ6Z2jwYB3gPOfrw=";
            ldflags = [ "-s" "-w" ];
            meta.mainProgram = "stuff";
          };
        });

      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.gopls pkgs.gotools pkgs.couchdb3 ];
          };
        });

      checks = forAllSystems (system: { build = self.packages.${system}.default; });
    };
}
