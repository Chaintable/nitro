# ApeChain image build

`apechain/build.sh` builds the image Caldera ships for ApeChain from the checked-out commit. Anyone with the same commit and an amd64 Linux host gets the same binaries and the same WASM machine. Image IDs still differ between hosts because Docker records build timestamps; compare module roots and `nitro --version`, not IDs.

## Image tag

`nitro-node:apechain-<version>-<short sha>`, e.g. `nitro-node:apechain-v3.9.9-6b534a4`. `<version>` is the upstream nitro release the branch is based on. The sha ties the image to its commit; the same value is in the `org.opencontainers.image.revision` label.

## Build

Native amd64 Linux only. QEMU emulation of the Rust/WASM stage segfaults. If the build host cannot reach GitHub, clone and `git submodule update --init --recursive` elsewhere, tar the tree including `.git`, and unpack it on the host. Delete macOS `._*` files before building; the script refuses to run if any exist. It also refuses a dirty tree or dirty submodules, since the sha in the tag would then not describe the build.

```
apechain/build.sh v3.9.9
```

After the build the script checks two things about the shipped machine: `module-root.txt` equals `EXPECTED_ROOT`, and `replay.wasm` contains ApeChain code. The second check is what catches an upstream machine masquerading as ours. Override `EXPECTED_ROOT` in the environment when a release intentionally changes the root.

## Carrying old machines

The staker needs the machine currently registered on L1, plus the new one during a `setWasmModuleRoot` transition. Pass the image currently in production as the second argument. `apechain/Dockerfile.validator` copies every machine it carries into the new image, its `latest` under its own root, skips duplicates, and re-runs the prover check. Passing it on every release is fine.

```
apechain/build.sh v3.9.9 001138754299.dkr.ecr.us-west-2.amazonaws.com/nitro-node:ape-yield-v3.9.9-fixed-validator
```

## Push

Same tag to both registries.

```
docker tag nitro-node:apechain-v3.9.9-6b534a4 001138754299.dkr.ecr.us-west-2.amazonaws.com/nitro-node:apechain-v3.9.9-6b534a4
docker push 001138754299.dkr.ecr.us-west-2.amazonaws.com/nitro-node:apechain-v3.9.9-6b534a4
docker tag nitro-node:apechain-v3.9.9-6b534a4 public.ecr.aws/i6b2w2n6/nitro-node:apechain-v3.9.9-6b534a4
docker push public.ecr.aws/i6b2w2n6/nitro-node:apechain-v3.9.9-6b534a4
```
