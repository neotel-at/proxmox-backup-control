# PVE Backup GUI

## 0.3.4 · 25. November 2026

- Journalmeldungen für Start, Wiederaufnahme und Abschluss von Backup-Läufen sowie bestätigten E-Mail-Versand.
- Neue Proxmox-Backups erhalten den Gastnamen in den Notes über `{{guestname}}`.

## 0.3.3 · 24. November 2026

- Expliziter Klartext-SMTP-Modus für interne Relays ohne Zugangsdaten. STARTTLS bleibt voreingestellt.

## 0.3.2 · Meilenstein v0.32 · 24. November 2026

- Fehlerbenachrichtigungen per E-Mail mit STARTTLS oder direkt verschlüsseltem SMTP. Pro Lauf werden Zustellung und Wiederholungen im Verlauf angezeigt.
- Versionsanzeige `v0.32` in der Seitenleiste. `pve-backup-gui -version` gibt die Paketversion `0.3.2` aus.
- Debian-Installationspaket mit systemd-Unit, Konfigurationsbeispiel und Apache-Vorlage. Bestehende Installationen können ihre Konfiguration und Zustandsdatei übernehmen.

Der Meilenstein umfasst die bisherigen Inventarfilter, sequenziellen Backuppläne, wiederkehrenden Termine, die Verlaufsbereinigung und die integrierte Benutzerverwaltung.
