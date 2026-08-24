# Security scope

Buntzen Bot is designed for one administrator on a trusted private LAN. Do not expose its HTTP port to the public Internet.

Security properties expected to hold:

- OTP provider selection is explicit and never falls back.
- BlueBubbles uses only ping and bounded message-query requests; Twilio never sends messages.
- Yodel/provider credentials and OTPs never enter durable events, rendered HTML responses/history, screenshots, traces, or stdout.
- The raw OTP exists only in the live Go/Python exchange and temporary SSE state.
- Final-confirmation ambiguity becomes `outcome_unknown`, never an automatic retry.
- One profile/source has at most one active job, and one appdata directory has at most one control plane.

The local encryption key is stored beside the database and does not protect a copied `/appdata` directory. LAN HTTP does not provide transport confidentiality. BlueBubbles’ server password itself is unscoped; read-only behavior is an application invariant rather than a BlueBubbles permission boundary.
