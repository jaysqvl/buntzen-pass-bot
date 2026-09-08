# Changelog

## [0.5.3](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.5.2...buntzen-pass-bot-v0.5.3) (2026-09-08)


### Bug Fixes

* select and verify saved Yodel vehicles and preserve booking failure details ([2a21572](https://github.com/jaysqvl/buntzen-pass-bot/commit/2a21572c69dfa1db2eaa51f67b18ca61e17b1f62))

## [0.5.2](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.5.1...buntzen-pass-bot-v0.5.2) (2026-09-08)


### Bug Fixes

* **release:** support latest images and Portainer-managed updates ([#73](https://github.com/jaysqvl/buntzen-pass-bot/issues/73)) ([1217160](https://github.com/jaysqvl/buntzen-pass-bot/commit/1217160ed2536c512ff3a9421082c5d8e2fbf1f9))

## [0.5.1](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.5.0...buntzen-pass-bot-v0.5.1) (2026-09-07)


This release hardens invited-user access through public HTTPS using Buntzen's own accounts. It includes operator-owned browser executables, removal of raw authenticated diagnostics, trusted connector and Secure-cookie enforcement, private bootstrap, bounded authentication and streams, and idle session expiry with reliable revocation.

Before upgrading, review [public HTTPS and migration settings](docs/public-exposure.md). Existing BlueBubbles sources require an explicit operator endpoint policy; old member executable overrides must be cleared. Public mode requires completed private bootstrap and the exact public origin/connector configuration. Containers now have finite resource limits and a read-only root. Failed or ambiguous Portainer updates stop for operator recovery instead of issuing an unsafe automatic rollback.

### Bug Fixes

* **security:** bound container resources and verify browser runtime ([e41aadb](https://github.com/jaysqvl/buntzen-pass-bot/commit/e41aadb2c2a9a11561a2426bd1554f1d6d240567))
* **security:** bound persistent profile storage inspection and growth ([2763424](https://github.com/jaysqvl/buntzen-pass-bot/commit/27634246ef3d74fa970c41769f2e4dcaed6bc7f9))
* **security:** bound worker execution and preserve verified outcomes ([68ddf26](https://github.com/jaysqvl/buntzen-pass-bot/commit/68ddf266a8e4e6bd97e265c814bbbfdcabd8110f))
* **security:** harden public HTTPS operation and release verification ([de97582](https://github.com/jaysqvl/buntzen-pass-bot/commit/de975829c79cc5fd890200f85829dfcc5a8c1913))
* **security:** pin build inputs and remove vulnerability waivers ([6ef2f5b](https://github.com/jaysqvl/buntzen-pass-bot/commit/6ef2f5b3357f9c9510110fab0b7fdcd756c03d59))
* **security:** preserve and validate existing encryption keys ([b48fc8f](https://github.com/jaysqvl/buntzen-pass-bot/commit/b48fc8f17ef0e9fbbe9443336f13695ff3c0ba02))
* **security:** restrict provider destinations and resolved peers ([280d719](https://github.com/jaysqvl/buntzen-pass-bot/commit/280d719ee35b7723d631a6aa76b5156c93cee451))
* **security:** verify running deployment identity and stop unsafe compensation ([4564bea](https://github.com/jaysqvl/buntzen-pass-bot/commit/4564beaea07cc2c2ada268a1ddf576e9d51c7b36))


### Documentation

* **security:** define public HTTPS posture and recovery boundaries ([745e60c](https://github.com/jaysqvl/buntzen-pass-bot/commit/745e60cc59934a107f63fea3070a551a5870b6db))

## [0.5.0](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.4.3...buntzen-pass-bot-v0.5.0) (2026-09-07)


### Features

* **web:** show the deployed release and build in the app ([#69](https://github.com/jaysqvl/buntzen-pass-bot/issues/69)) ([c1b4bb9](https://github.com/jaysqvl/buntzen-pass-bot/commit/c1b4bb9df29064f382364e3094edc6401c2c0b4e))

## [0.4.3](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.4.2...buntzen-pass-bot-v0.4.3) (2026-09-07)


### Bug Fixes

* **web:** keep action failures inside the application ([#67](https://github.com/jaysqvl/buntzen-pass-bot/issues/67)) ([52c4ec7](https://github.com/jaysqvl/buntzen-pass-bot/commit/52c4ec74d96d618bfdc018e1ee301daf53b11b4f))

## [0.4.2](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.4.1...buntzen-pass-bot-v0.4.2) (2026-09-06)


### Bug Fixes

* **web:** clarify queued bookings and automatic confirmation ([#65](https://github.com/jaysqvl/buntzen-pass-bot/issues/65)) ([7aa52ea](https://github.com/jaysqvl/buntzen-pass-bot/commit/7aa52ea81e2489c47f3e9b6af4292797d12f27cf))

## [0.4.1](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.4.0...buntzen-pass-bot-v0.4.1) (2026-09-06)


### Bug Fixes

* **web:** return source and profile flows to Setup ([#63](https://github.com/jaysqvl/buntzen-pass-bot/issues/63)) ([e4f312e](https://github.com/jaysqvl/buntzen-pass-bot/commit/e4f312e04c416c3a605c154e6bc7dbde952c6346))

## [0.4.0](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.3.2...buntzen-pass-bot-v0.4.0) (2026-09-06)


### Features

* clarify setup and enable immediate manual checkout ([#61](https://github.com/jaysqvl/buntzen-pass-bot/issues/61)) ([32f602a](https://github.com/jaysqvl/buntzen-pass-bot/commit/32f602af52323b36b228e0b6bd4f118db717b795))

## [0.3.2](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.3.1...buntzen-pass-bot-v0.3.2) (2026-09-05)


### Bug Fixes

* drain action pipes before waiting ([6278a65](https://github.com/jaysqvl/buntzen-pass-bot/commit/6278a65dc3bf151a7a072350239a94142368409e))
* drain action pipes before waiting ([2b0f8b0](https://github.com/jaysqvl/buntzen-pass-bot/commit/2b0f8b085ecd00a46a9a26abc328e6e0ef16d211))
* verify bookings and simplify runtime ownership ([#60](https://github.com/jaysqvl/buntzen-pass-bot/issues/60)) ([19b3ad7](https://github.com/jaysqvl/buntzen-pass-bot/commit/19b3ad78e0ac093e5cb32dd9e47d5552f690f5da))

## [0.3.1](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.3.0...buntzen-pass-bot-v0.3.1) (2026-08-25)


### Bug Fixes

* accept SLSA v1 build provenance ([205c2aa](https://github.com/jaysqvl/buntzen-pass-bot/commit/205c2aa61f530b435097c93b4451a64ea0d9c048))
* accept SLSA v1 build provenance ([167ecd5](https://github.com/jaysqvl/buntzen-pass-bot/commit/167ecd5037de1a5a5c478046e9a067eaf83da8fd))
* keep draining oversized action stderr ([8b2c16c](https://github.com/jaysqvl/buntzen-pass-bot/commit/8b2c16c7cee474ecb5125e49ad062991b97aaa1d))
* keep draining oversized action stderr ([8133aed](https://github.com/jaysqvl/buntzen-pass-bot/commit/8133aed76d9f30628acd6b3ad88990cf1030103c))
* publish component-prefixed releases ([107cc58](https://github.com/jaysqvl/buntzen-pass-bot/commit/107cc58572a00a152ecf5c4723f7f98b653e8385))
* publish component-prefixed releases ([1d06aaf](https://github.com/jaysqvl/buntzen-pass-bot/commit/1d06aaf5a4389906c28ff02c3cf99cdd8531af3b))

## [0.3.0](https://github.com/jaysqvl/buntzen-pass-bot/compare/buntzen-pass-bot-v0.2.0...buntzen-pass-bot-v0.3.0) (2026-08-25)


### Features

* add development observability and current Yodel OTP flow ([99d1404](https://github.com/jaysqvl/buntzen-pass-bot/commit/99d1404b5d40a91c465675544ca0556536f1f3fc))
* add isolated user accounts ([c842c37](https://github.com/jaysqvl/buntzen-pass-bot/commit/c842c37d0984569f8cd68a03ec096657bbfd3fb8))
* add isolated user accounts ([c3cf06c](https://github.com/jaysqvl/buntzen-pass-bot/commit/c3cf06c828486f117b881d26560f13a8dd478b55))
* add safe development observability ([0211639](https://github.com/jaysqvl/buntzen-pass-bot/commit/0211639260c7e67ebd9fbd4b2771a1506bbe62a9))
* rebuild Buntzen as Go control plane ([35f5407](https://github.com/jaysqvl/buntzen-pass-bot/commit/35f5407ee568e4c0a526e3e85d4c06b89b05912e))


### Bug Fixes

* align Yodel mobile login and diagnostics ([6ab0006](https://github.com/jaysqvl/buntzen-pass-bot/commit/6ab0006f8244d9b9203d4dc1a6feabc41fc043bf))
* keep release metadata consistent ([553b40e](https://github.com/jaysqvl/buntzen-pass-bot/commit/553b40e2557de889a7ba0996239263a9ad476f62))
* keep release metadata consistent ([e863819](https://github.com/jaysqvl/buntzen-pass-bot/commit/e863819725ee4283e178668d216c9376b221acb3))
* make release keepalive independent of uptime ([a9c2696](https://github.com/jaysqvl/buntzen-pass-bot/commit/a9c26963ec260a77cf280cfc2621b55d917df8ae))
