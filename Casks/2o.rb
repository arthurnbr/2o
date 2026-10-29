cask "2o" do
  arch arm: "arm64", intel: "amd64"
  os macos: "darwin", linux: "linux"

  version "0.2.0"
  sha256 arm:          "b31c3fcc2451e3555f55e5d83368a75a4bb0122f4c86620f45b7ebcaef0e9416",
         intel:        "0c946062f6298f952206d655f613f5332b7a6f4234a17c286f8c9e58d44dfa06",
         arm64_linux:  "a57b6c91226bd35de3571be66d4263102643d8eac5d97b1cb46ae505a7658da0",
         x86_64_linux: "5fc7ea450cd0b8645e6994a0b8c86387a0b608299fb45995fb821cba02c08a5c"

  url "https://github.com/arthurnbr/2o/releases/download/v#{version}/2o_#{version}_#{os}_#{arch}.tar.gz"
  name "2o"
  desc "Open files and URLs on your local desktop from a remote SSH shell"
  homepage "https://github.com/arthurnbr/2o"

  livecheck do
    url :url
    strategy :github_latest
  end

  # 2o-linux-{amd64,arm64} stay next to 2o: `2o ssh` uploads them to servers.
  binary "2o_#{version}_#{os}_#{arch}/2o"

  # Unsigned binary: without this, Gatekeeper blocks the downloaded copy.
  postflight_steps do
    on_macos do
      run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{staged_path}}"]
    end
  end

  zap trash: "~/.cache/2o"
end
