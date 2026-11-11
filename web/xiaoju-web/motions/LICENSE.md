# Licenses / ライセンス

This dataset mixes motions from several sources. **Each motion's license is written in its own `<name>.motion.json` under `license`** (SPDX id, attribution text, author, source). The rules for each source are below.

このデータセットには出どころの違うモーションが入っています。**1本ごとのライセンスは、隣にある `<name>.motion.json` の `license` に書いてあります**（SPDX の識別子、そのまま貼れるクレジット文、作者、出典）。素材ごとの条件は次のとおりです。

## `cmu/` — CMU Graphics Lab Motion Capture Database

- `license.spdx`: `LicenseRef-CMU-Mocap`
- Terms (from http://mocap.cs.cmu.edu/ and http://mocap.cs.cmu.edu/faqs.php):
  - "The motion capture data may be copied, modified, or redistributed without permission."
  - "You may include this data in commercially-sold products, but you may not resell this data directly, even in converted form."
  - "Please don't crawl this database for all motions." — get the data from this dataset or the GitHub mirror instead of crawling mocap.cs.cmu.edu.
- 使ってよいこと：複製・改変・再配布、商用の作品や製品に含めること。
- してはいけないこと：データそのものを（このデータセットの VRMA のように変換した形でも）販売すること。
- Please credit / 作品には次の謝辞を入れてください:

  > The data used in this project was obtained from mocap.cs.cmu.edu. The database was created with funding from NSF EIA-0196217.

- The BVH files used as input are the 2010 Motionbuilder-friendly conversion by Bruce Hahne (cgspeed), who placed no additional restrictions on them, obtained through the GitHub mirror https://github.com/Shriinivas/cmubvh.

## `quaternius/ual1/`, `quaternius/ual2/` — Quaternius Universal Animation Library 1 & 2 (Standard)

- `license.spdx`: `CC0-1.0` (https://creativecommons.org/publicdomain/zero/1.0/)
- Public domain. No attribution required; credit "Animations by Quaternius (https://quaternius.com/)" is appreciated.
- パブリックドメインです。クレジットは不要ですが、書く場合は上の文をどうぞ。

## `kaykit/medium/`, `kaykit/large/` — KayKit Character Animations 1.1

- `license.spdx`: `CC0-1.0` (https://creativecommons.org/publicdomain/zero/1.0/)
- By Kay Lousberg (https://www.kaylousberg.com/), from https://kaylousberg.itch.io/kaykit-character-animations. Public domain. No attribution required; credit "Animations by Kay Lousberg" is appreciated.
- パブリックドメインです。クレジットは不要です。

## The conversion, tags and metadata / 変換・タグ・メタデータ

The VRMA conversion, the `.motion.json` files and the catalogs made by the open-vrma tools are dedicated to the public domain under **CC0 1.0**. This does not change the terms of the underlying motion data above.

open-vrma のツールで作った変換結果の構造・`.motion.json`・一覧は **CC0 1.0** とします。元のモーションデータの条件（上記）はそのまま適用されます。

## Disclaimer / 免責

The data is provided as is, without warranty. Tags, tempo and timing were estimated automatically and may be wrong.
データは現状のまま提供し、保証はありません。タグ・テンポ・タイミングは自動推定なので、誤りを含みます。
