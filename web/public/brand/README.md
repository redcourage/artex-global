# CryptoLab ARTEX web brand assets

The light and dark symbols were copied unchanged from the CryptoLab HEaaN
light/dark SVG artwork. The wordmark is rendered as CryptoLab ARTEX.
The original CODE.HEAAN wordmark is not embedded.

The application uses `/brand/cryptolab-light.svg` and
`/brand/cryptolab-dark.svg` in normal browsers. Web logos, favicon and app icon
assets are committed in `web/public` and `web/src/app`.
No native macOS bundle or desktop icon-generation script is needed to run or
build this web frontend.

To generate the standalone theme preview, run from `web/`:

```sh
node scripts/build-theme-lab.mjs
```

The default output is `web/out/theme-lab.html`. An explicit output path can be
passed as the first argument. The preview uses fictional sessions and makes no
model requests.
