cask "2o" do
  arch arm: "arm64", intel: "amd64"
  os macos: "darwin", linux: "linux"

  version "0.1.0"
  sha256 arm:          "0000000000000000000000000000000000000000000000000000000000000000",
         intel:        "0000000000000000000000000000000000000000000000000000000000000000",
         arm64_linux:  "0000000000000000000000000000000000000000000000000000000000000000",
         x86_64_linux: "0000000000000000000000000000000000000000000000000000000000000000"

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
