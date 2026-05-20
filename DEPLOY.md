# Deploy

1. Point DNS records:
   - `one2one.dev.gloud.com.ua` to the server running Traefik.
   - `turn.dev.gloud.com.ua` to the same server public IP.

2. Create `.env`:

```env
TURN_EXTERNAL_IP=YOUR_SERVER_PUBLIC_IP
TURN_USERNAME=one2one
TURN_CREDENTIAL=CHANGE_ME_LONG_RANDOM_SECRET
```

3. Open firewall ports for coturn:

```text
3478/tcp
3478/udp
5349/tcp
5349/udp
49152-65535/udp
```

4. Start:

```bash
docker compose up -d --build
```

The app reads `/api/config` in the browser and uses the TURN settings from the container environment.
