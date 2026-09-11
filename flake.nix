{
  description = "Discord channels as readable Git snapshots";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in {
      packages = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.buildGoModule {
            pname = "discord-to-git";
            version = "0.1.0";
            meta = {
              description = "Incremental Discord channel archives as JSON files in Git";
              homepage = "https://github.com/nishu-builder/discord-to-git";
              license = pkgs.lib.licenses.mit;
              mainProgram = "discord-to-git";
            };
            src = self;
            vendorHash = null; # The program uses only Go's standard library.
            env.CGO_ENABLED = "0";
            nativeBuildInputs = [ pkgs.git pkgs.makeWrapper ];
            doCheck = true;
            preCheck = ''
              export HOME="$TMPDIR/home"
              mkdir -p "$HOME"
            '';
            postInstall = ''
              wrapProgram $out/bin/discord-to-git \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.git pkgs.openssh ]} \
                --set-default SSL_CERT_FILE ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
            '';
          };
        });
      checks = eachSystem (system: { build = self.packages.${system}.default; });
      devShells = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.gopls pkgs.git ];
          };
        });
    };
}
