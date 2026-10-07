# BCH2 Community Apps: Umbrel App Store

The official **Umbrel community app store** for BCH2 apps (currently **Forge Solo**).

## Add this store to Umbrel

In Umbrel: **Settings → App Store → Community App Stores → Add**, and paste this URL:

```
https://github.com/BitcoincashII/umbrel-app-store
```

Then open **BCH2 Community Apps** and install the app.

> **Add THIS repo, not the core repo.**
> `github.com/BitcoincashII/bitcoincashII-core` is the BCH2 **node/wallet source code**, *not* an app store. Adding it makes Umbrel fail with `Failed to read registry ... no such file ... umbrel-app-store.yml`. This repo (`umbrel-app-store`) is the only one to add.

## Apps

- **Forge Solo**: mine BCH2 at home on your own full node, solo (a block you find pays you in full) or TIDES (blocks shared through Forge Pool's TIDES window), with no pool fee either way. In solo mode it also merge-mines 1175 (ESF) at no extra hashrate cost. Set your payout address in the app, then point your ASIC or Bitaxe at `stratum+tcp://<your-umbrel-ip>:3333`. Rentals from NiceHash and MiningRigRentals use port 3335. Forge Solo also runs on Windows and Linux, built from the same repository, [BitcoincashII/forge-solo](https://github.com/BitcoincashII/forge-solo); 1175 merge-mining is on Umbrel and Windows.
