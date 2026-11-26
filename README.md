# PVE Backup GUI

Eine eigenständige Go-Webanwendung für Proxmox VE. Sie listet QEMU-VMs und LXC-Container pro Cluster, speichert die Auswahl samt Reihenfolge und startet pro Gast genau einen Proxmox-Backup-Task im Snapshot-Modus. Ein Plan gehört zu einem Cluster. Die Warteschlange führt Läufe dieser Instanz clusterübergreifend nacheinander aus. Bereits vorhandene Proxmox-Backupjobs laufen unabhängig.

## Lokal ausprobieren

Linux und Go ab 1.23 genügen. Das Frontend ist in die ausführbare Datei eingebettet. Die Go-Abhängigkeiten sind über `vendor/` mitgeliefert.

```sh
go build -o pve-backup-gui .
./pve-backup-gui -demo
```

Öffnen Sie [http://127.0.0.1:8080](http://127.0.0.1:8080). Die Demo-Anmeldung lautet `demo` mit Passwort `pve-backup-demo`. Im Demo-Modus gibt es zwei simulierte Cluster. VM 115 erzeugt einen Fehler. Die Demo speichert ihren Verlauf unter `data/demo.json`.

## Proxmox verbinden

1. Kopieren Sie `config.example.json` nach `/etc/pve-backup-gui/config.json`. Hinterlegen Sie pro Cluster einen erreichbaren PVE-Knoten per HTTPS, den Namen, eine stabile Cluster-ID und einen API-Token. Die Cluster-ID muss dem gleichen Cluster zugeordnet bleiben, solange gespeicherte Pläne oder Läufe existieren.
2. Wenn der PVE-Endpunkt ein von der Proxmox-Cluster-CA ausgestelltes Zertifikat nutzt, kopieren Sie `/etc/pve/pve-root-ca.pem` vom Cluster zum GUI-Host und tragen Sie den Pfad als `ca_file` ein. Ein öffentlich vertrauenswürdiges Zertifikat benötigt keinen Eintrag. Der Hostname muss zum Zertifikat passen.
3. Verwenden Sie einen eigenen Proxmox-Benutzer und einen API-Token mit Privilege Separation. Vergeben Sie die folgenden Rechte beim Benutzer und beim Token. Bei vererbten ACLs beeinflusst der ACL-Pfad die Gäste und Storages, die in der Oberfläche sichtbar sind.

| Pfad | Rechte |
| --- | --- |
| `/vms` mit Vererbung oder einzelne `/vms/<vmid>` | `VM.Audit` für das Inventar, `VM.Backup` für den Backupstart |
| `/storage/<storage>` je freigegebenem Ziel | `Datastore.Audit` und `Datastore.AllocateSpace` |
| `/nodes/<node>`, falls fremde Tasks gelesen werden sollen | `Sys.Audit`. Die Anwendung liest die Tasks ihres Tokens. Nach einem Tokenwechsel kann dies für ältere Tasks relevant sein. |

Legen Sie für jeden beabsichtigten Storage in Proxmox den Inhaltstyp **Backup** fest, etwa auf Proxmox Backup Server oder auf einem geeigneten Dateispeicher. Die GUI verwendet eine Proxmox-Storage-ID als Ziel. Bei Gästen auf mehreren Knoten stehen nur gemeinsame, aktive Storages zur Wahl. Ein lokales Ziel bleibt an den Knoten gebunden, auf dem es ausgewählt wurde. Jeder Aufruf sendet `remove=0` und `notes-template={{guestname}}`. Proxmox setzt den Gastnamen in die Notes des neu erstellten Backups ein. Die GUI verwaltet keine Aufbewahrungsregeln.

API-Referenzen von Proxmox

- [Backupstart](https://pve.proxmox.com/pve-docs/api-viewer/#/nodes/{node}/vzdump)
- [Taskstatus und Logs](https://pve.proxmox.com/pve-docs/api-viewer/#/nodes/{node}/tasks)
- [API-Tokens](https://pve.proxmox.com/pve-docs/chapter-pveum.html#pveum_tokens)

## Debian-Paket installieren

Das Release-Paket `pve-backup-gui_0.3.5_amd64.deb` ist für Debian auf x86-64. Für andere unterstützte Architekturen kann das Paket aus dem Quellpaket gebaut werden.

```sh
sudo apt install ./pve-backup-gui_0.3.5_amd64.deb
sudo install -m 0640 -o root -g pve-backup-gui \
  /usr/share/doc/pve-backup-gui/examples/config.example.json /etc/pve-backup-gui/config.json
sudoedit /etc/pve-backup-gui/config.json /etc/pve-backup-gui/secret.env
sudo -u pve-backup-gui /usr/bin/pve-backup-gui -config /etc/pve-backup-gui/config.json -init-user backupadmin
sudo systemctl enable --now pve-backup-gui
```

Den `install`-Befehl für die JSON-Datei nur bei einer Neuinstallation verwenden. Das Paket legt Dienstkonto, Datenverzeichnis und bei Bedarf eine leere `secret.env` an. Es startet den Dienst bei der Erstinstallation erst auf explizite Anweisung. Die Apache-Vorlage steht unter `/usr/share/doc/pve-backup-gui/examples/apache.conf`. Passen Sie Hostname und Zertifikatspfade an, bevor Sie sie als HTTPS-VirtualHost aktivieren.

### Von einer manuellen Installation umsteigen

Konfiguration, `secret.env` und `state.json` sichern. Läuft noch ein Backup, warten Sie nach Möglichkeit dessen Ende ab. Prüfen Sie mit `systemctl cat pve-backup-gui` den Pfad der geladenen Unit und vorhandene Overrides. Stoppen und deaktivieren Sie danach den Dienst. Liegt die bisherige Unit unter `/etc/systemd/system/pve-backup-gui.service`, verschieben Sie sie als Sicherung aus diesem Pfad, damit systemd die Unit des Pakets laden kann. Prüfen Sie bei einem Drop-in, ob es einen früheren `ExecStart` überschreibt.

Installieren Sie das Paket, behalten Sie Ihre aktuelle Konfiguration und laden Sie systemd neu. Bei einer Zustandsdatei mit bereits eingerichteten GUI-Konten melden Sie sich wie bisher an. Prüfen Sie mit `systemctl cat pve-backup-gui`, dass die Unit `/usr/bin/pve-backup-gui` startet. Aktivieren Sie dann den Dienst mit `sudo systemctl enable --now pve-backup-gui`. Eine vorhandene Apache-HTTPS-Konfiguration kann weiter genutzt werden.

Bei der Deinstallation bleiben Konfiguration und Zustandsdaten für Wiederherstellungen erhalten.

### Debian-Paket bauen

```sh
./deploy/debian/build.sh .
```

Der Build benötigt Go ab 1.23 und `dpkg-deb`. `DEB_ARCH=arm64` wählt einen ARM64-Build.

## Betrieb mit systemd und Apache

Für eine Debian-Installation mit Apache 2.4

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pve-backup-gui
sudo install -m 0755 pve-backup-gui /usr/local/bin/pve-backup-gui
sudo install -d -m 0750 -o root -g pve-backup-gui /etc/pve-backup-gui
sudo install -m 0640 -o root -g pve-backup-gui config.example.json /etc/pve-backup-gui/config.json
sudo install -m 0600 /dev/null /etc/pve-backup-gui/secret.env
sudoedit /etc/pve-backup-gui/config.json /etc/pve-backup-gui/secret.env
sudo install -d -m 0700 -o pve-backup-gui -g pve-backup-gui /var/lib/pve-backup-gui
sudo -u pve-backup-gui /usr/local/bin/pve-backup-gui -config /etc/pve-backup-gui/config.json -init-user backupadmin
sudo install -m 0644 deploy/pve-backup-gui.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pve-backup-gui
```

Tragen Sie in `secret.env` pro Cluster den in `secret_env` angegebenen Variablennamen und das Token-Secret ein, z. B. `PVE_PROD_TOKEN=…`. Der systemd-Dienst erstellt das Datenverzeichnis. Beim direkten Start übernimmt die Shell die Umgebungsvariablen. Der Go-Dienst lauscht ausschließlich auf Loopback.

Die Apache-Vorlage leitet HTTPS an den lokalen Go-Dienst weiter. Passen Sie den Hostnamen in beiden VirtualHosts, das Ziel der HTTP-Weiterleitung und die TLS-Zertifikatspfade an. Im Produktivbetrieb wird der Sitzungscookie ausschließlich über HTTPS übertragen. Die GUI erwartet einen eigenen Hostnamen und das URL-Wurzelverzeichnis.

```sh
sudo a2enmod ssl proxy proxy_http
sudo install -m 0644 deploy/apache.conf /etc/apache2/sites-available/pve-backup-gui.conf
sudo a2ensite pve-backup-gui
sudo apachectl configtest
sudo systemctl reload apache2
```

Rufen Sie im Browser die HTTPS-Adresse des Reverse Proxy auf. Beim Aufruf von `/` erscheint das Anmeldeformular. `/api/dashboard` antwortet ohne Anmeldung mit HTTP 401.

Meldet die GUI nach korrektem Passwort eine fehlende Sitzung, prüfen Sie im Netzwerk-Tab des Browsers die Anfragen `POST /api/login` und `GET /api/session`. Die Login-Antwort muss den Cookie `__Host-pve_session` mit `Secure` und `Path=/` setzen. Der Browser muss ihn bei der folgenden Sitzungsabfrage an denselben HTTPS-Host senden. Der Reverse Proxy muss `Set-Cookie` und `Cookie` weiterleiten. Verwenden Sie für den Cookie kein `Domain`-Attribut.

## Benutzer

Unter **Benutzer** können angemeldete Personen Konten anlegen, Passwörter ändern und andere Konten löschen. Alle Konten haben dieselben Rechte, einschließlich der Verwaltung von Backupplänen, Verlauf und Benutzerkonten. Passwörter benötigen mindestens 15 Zeichen. Gelöschte Konten und Konten mit geändertem Passwort verlieren ihre bisherigen Sitzungen. Nach einem Dienstneustart ist eine erneute Anmeldung nötig.

Wenn der Zugang zu allen Konten verloren geht, stoppen Sie den Dienst und führen Sie den Ersteinrichtungsbefehl mit `-reset-user backupadmin` statt `-init-user backupadmin` aus. Danach den Dienst starten.

## Pläne, Läufe und Logs

- Im **Inventar** Cluster und Gäste wählen, Reihenfolge über die Pfeil-Schaltflächen festlegen, Ziel wählen und den Plan speichern. **Nur laufende Gäste** blendet gestoppte VMs und Container aus. **Nur Gäste ohne Backupplan** zeigt Gäste, die in keinem gespeicherten Plan dieses Clusters enthalten sind. Beide Filter lassen sich kombinieren. Bereits ausgewählte Gäste bleiben in der Backup-Reihenfolge. **Alle sichtbaren Gäste auswählen** bezieht sich auf die aktuelle Suche und den Statusfilter.
- Unter **Backuppläne** den Plan sofort starten oder mit **Terminieren** einen einmaligen Termin, eine tägliche Serie oder eine Serie für ausgewählte Wochentage einrichten. Jeder Plan kann eine aktive Serie haben. Unter **Terminplan** können Sie die Serie ändern, löschen oder zusätzlich einen Einmaltermin setzen.
- Einmaltermine verwenden die Zeitzone des Browsers. Serien verwenden standardmäßig `Europe/Vienna`. Die optionale Einstellung `schedule_timezone` bestimmt die IANA-Zeitzone für neue Serien. Bestehende Serien behalten ihre gespeicherte Zeitzone. Bei der Umstellung auf Sommerzeit entfällt ein Termin in einer übersprungenen Stunde. Bei der Rückstellung auf Normalzeit gilt bei einer doppelt vorkommenden Uhrzeit der erste Zeitpunkt.
- Ist der Dienst zum Termin aus, holt er fällige Einmaltermine beim nächsten Start nach. Bei Serien fasst er verpasste Termine zu höchstens einem wartenden Serienlauf pro Plan zusammen. Änderungen am Backupplan gelten für künftige Läufe. Eingereihte Läufe enthalten eine Kopie des Plans und können im Verlauf abgebrochen werden.
- Im **Verlauf** können Sie bereits eingereihte Läufe abbrechen und die Proxmox-Task-Logs pro Gast öffnen. Abgeschlossene Logs werden mit bis zu 10.000 Zeilen pro Task lokal archiviert. Status, Verlauf und archivierte Logs liegen in der Zustandsdatei. **Aus Verlauf löschen** entfernt einen abgeschlossenen Lauf samt lokalen Logs. **Verlauf bereinigen** löscht abgeschlossene Einträge, deren Laufende mehr als die gewählte Anzahl an Tagen zurückliegt. Läufe mit ungeklärtem Ausgang sind nach der Freigabe der Warteschlange löschbar. Beide Funktionen bearbeiten ausschließlich den lokalen GUI-Verlauf.
- Standardmäßig überspringt der Dienst nach einem bestätigten Fehler die übrigen Gäste im Lauf. Im Plan können Sie die Fortsetzung aktivieren. Kann der Dienst den Ausgang eines Tasks nicht feststellen, pausiert er die Warteschlange. Prüfen Sie den Task in Proxmox, bevor Sie die Warteschlange im Verlauf wieder freigeben. Nach einem Dienstneustart überwacht er Tasks mit bekannter UPID weiter.
- Betreiben Sie eine Instanz für eine Zustandsdatei. Sichern Sie die Datei regelmäßig. Ein wiederhergestellter älterer Stand kann zuvor eingereihte Läufe und fällige Serien enthalten. Prüfen Sie diese vor der Wiederaufnahme der Verarbeitung.

## Fehlerbenachrichtigung

Tragen Sie in `webhook_env` beispielsweise `PVE_FAILURE_WEBHOOK` ein und hinterlegen Sie in `secret.env` unter diesem Namen die HTTPS-URL eines Empfängers. Der Dienst sendet pro fehlgeschlagenem oder unklarem Lauf ein JSON-Ereignis `pve_backup_attention` mit Lauf-ID, Plan und Ergebnissen. Eine erfolgreiche HTTP-Antwort liegt zwischen 200 und 299. Bei Fehlern folgen bis zu vier Wiederholungen. Der Empfänger kann mit `Idempotency-Key` deduplizieren. Für E-Mail oder Chat kann ein vorhandener Webhook-Empfänger das Ereignis entsprechend weiterleiten.

## E-Mail bei Backupfehlern

Ergänzen Sie in der vorhandenen `config.json` auf oberster Ebene einen `smtp`-Block, zum Beispiel:

```json
"smtp": {
  "host": "smtp.example.net",
  "port": 587,
  "tls_mode": "starttls",
  "username": "backup@example.net",
  "password_env": "PVE_SMTP_PASSWORD",
  "from": "PVE Backup <backup@example.net>",
  "to": ["betrieb@example.net"]
}
```

Hinterlegen Sie `PVE_SMTP_PASSWORD` in `/etc/pve-backup-gui/secret.env` und starten Sie den Dienst neu. Standardmäßig verwendet der Versand STARTTLS auf Port 587. Für direkte TLS-Verbindungen wählen Sie `"tls_mode": "tls"` mit Port 465. Bei einem internen SMTP-Zertifikat kann `ca_file` eine zusätzliche vertrauenswürdige CA angeben. Ein SMTP-Relay ohne Anmeldung braucht weder `username` noch `password_env`. Bei SMTP-Anmeldung muss der Server `AUTH PLAIN` über die TLS-Verbindung unterstützen.

Für einen internen SMTP-Relay in einem vertrauenswürdigen Netz kann `"tls_mode": "plain"` gesetzt werden. Der Standardport für diesen Modus ist 25. Lassen Sie `username`, `password_env` und `ca_file` weg. Die Nachricht einschließlich Backup-Fehlerdetails wird in diesem Modus unverschlüsselt an den Relay übertragen.

Mit **Mail on error** sendet die GUI pro fehlgeschlagenem oder ungeklärtem Lauf eine Nachricht mit Plan, Cluster, Ziel, Lauf-ID und den betroffenen Gästen. Der Verlauf zeigt den Versandstatus. Bei Zustellfehlern folgen bis zu vier Wiederholungen. Webhook und E-Mail können gleichzeitig konfiguriert werden. Die Option ist pro Backupplan einstellbar und bei bestehenden Plänen standardmäßig aktiv. Für durch die GUI gestartete Backups unterdrückt die GUI Proxmox-eigene Backup-Benachrichtigungen, damit Proxmox keine zusätzlichen Meldungen pro Gast versendet. Weglassen von `smtp` oder `"smtp": null` deaktiviert E-Mail.

## Dienstjournal

```sh
sudo journalctl -u pve-backup-gui -f
```

Das Journal zeigt Start, Wiederaufnahme und Ergebnis von Backup-Läufen mit Lauf-ID und Planname. Bei einem Lauf aus einer Terminserie steht im Startprotokoll `serie=true`. Bei erfolgreichem E-Mail-Versand meldet der Dienst die Bestätigung durch den SMTP-Relay.

## Upgrade von 0.3.3

Installieren Sie das Debian-Update 0.3.4 oder ersetzen Sie das manuell installierte Programm und starten Sie den Dienst neu. Journalmeldungen und die Notes-Vorlage sind automatisch aktiv.

## Upgrade von 0.3.2

Installieren Sie das Debian-Paket 0.3.3 oder ersetzen Sie das manuell installierte Programm. Für interne Relays kann danach `"tls_mode": "plain"` in der JSON-Konfiguration gesetzt werden. Laden Sie die Konfiguration durch einen Dienstneustart.

## Upgrade von 0.3.1

Das Programm durch den Build von 0.3.2 ersetzen und den Dienst neu starten oder auf das Debian-Paket umsteigen. Für E-Mail ist der optionale `smtp`-Block erforderlich.

## Upgrade von 0.3.0

Ersetzen Sie das Programm durch den neuen Build und starten Sie den Dienst neu. Bestehende GUI-Konten liegen in `state.json`. Die HTTP-Weiterleitung im Beispiel für Apache kann bei Bedarf separat übernommen werden.

## Upgrade von 0.1.0 oder 0.2.0

1. Bauen Sie die neue Version. Stoppen Sie den Dienst, sichern Sie die vorhandene `state.json` und ersetzen Sie das Programm. Verwenden Sie dieselbe Konfiguration und Zustandsdatei.
2. Richten Sie mit `-init-user` wie oben ein erstes GUI-Konto ein. Weitere Konten legen Sie nach der Anmeldung in der GUI an.
3. Übernehmen Sie die neue Apache-Vorlage für diesen VirtualHost oder entfernen Sie dort die bisherigen Basic-Auth-Direktiven (`AuthType`, `AuthName`, `AuthBasicProvider`, `AuthUserFile` und `Require valid-user`). Prüfen Sie mit `apachectl configtest`, laden Sie Apache neu und starten Sie den GUI-Dienst.
4. Öffnen Sie die HTTPS-Adresse und melden Sie sich mit dem neuen GUI-Konto an.

## Tests

```sh
go test -race ./...
go vet ./...
```
