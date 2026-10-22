# 2o — revue multi-aspects (eduvia/gpt-6-astra, HEAD ebe7007)

Six reviewers en lecture seule : sécurité, qualité de code, features, fonctionnement, UX, UI des réponses. Les constats ci-dessous sont dédupliqués et classés ; chacun a été appuyé par une ligne de code ou une reproduction (essais réels sur 2serv-1 dans des répertoires jetables). Rapports complets : `agent://Securite2`, `agent://QualiteCode`, `agent://Features`, `agent://Fonctionnement2`, `agent://UX`, `agent://UIReponses`.

## P0 — à corriger d'abord (perte de données, effets de bord, sécurité)

| # | Problème | Preuve | Correction |
|---|---|---|---|
| 1 | `2o ssh HOST cmd` réexécute `cmd` jusqu'à 3 fois si elle sort en 255 en moins de 5 s | ssh.go:233-237 ; compteur = 3 sur 2serv-1 | Ne réessayer que si le forward a échoué *avant* le lancement de la commande (marqueur de démarrage), jamais après |
| 2 | `edit` : si l'éditeur sauvegarde puis sort ≠ 0, la copie modifiée est supprimée | main.go:467-507 ; `RECOVERY_DIRECTORIES []` | Garder la copie dès le lancement de l'éditeur, afficher son chemin, ne la supprimer qu'après commit distant confirmé |
| 3 | Handshake HMAC rejouable : nonce uniquement côté client, preuve non liée à la requête → une transcription capturée permet de rejouer et changer `ping` en `paste` | session.go:64-69,210-226 ; probe : preuve rejouée acceptée | Challenge serveur frais + preuve liée aux deux nonces, à l'ID et à l'en-tête de requête |
| 4 | Bombe TAR sparse : limites appliquées aux octets transportés, pas aux octets écrits (×6553 mesuré) ; la réserve disque n'est pas réservée entre transferts simultanés | files.go:270-309, policy.go:81-93 ; `announced=10240 extracted=67108864` | Refuser les entrées sparse ou les compter à leur taille développée ; budget cumulé d'extraction ; réservation atomique des octets engagés |
| 5 | Le pont localhost est ouvert à tous les utilisateurs du desktop, sans auth ni contrôle de `Host` (DNS rebinding possible) | proxy.go:52,77-89 ; `untrusted Host: 200 private upstream data` | Jeton aléatoire par pont (URL d'amorçage → cookie), validation stricte de `Host` |
| 6 | Session distante : `mkdir … \|\| test -d` accepte un répertoire préexistant et `cat >` suit les liens | ssh.go:186-209 | Création exclusive, échec si le chemin existe, pas de suivi de lien |
| 7 | Mémoire non bornée : le presse-papiers est lu entièrement avant le contrôle de taille (1 MiB chargé avec une limite de 8 o) | main.go:308-315, desktop.go:202-211 | Lecture limitée pendant la capture ; tuer l'outil au dépassement ; borner aussi la relecture |

## P1 — robustesse et contrat SSH

| # | Problème | Preuve | Correction |
|---|---|---|---|
| 8 | Options SSH mal parsées : `-L/-R/-D/-e/-B/-I/-O/-P/-w`, options groupées `-vp 2222` → la valeur devient l'hôte | ssh.go:22-45 ; `-L 8080:… server` → host `8080:localhost:80` | Table complète des options à valeur, options groupées/accolées ; refuser les modes incompatibles |
| 9 | Options utilisateur appliquées aussi au bootstrap (`-n`, `-N` cassent la préparation) ; `-T` forcé même si `-t` demandé | ssh.go:48-54,137-225 | Séparer options de transport et options de session ; respecter le choix PTY |
| 10 | Codes de sortie perdus : `exit 42` distant → 1 + « exit status 42 » | main.go:21-24, ssh.go:242 | Propager `ExitError.ExitCode()`, réserver un code aux erreurs propres à 2o, ne pas réafficher |
| 11 | Le repli tmux peut envoyer à **un autre bureau** (session la plus récente du compte) sans prévenir ; `status` n'indique pas la destination | session.go:145-174 | Identité du bureau dans la session et dans `status` ; avertir/refuser si la destination change ; choix explicite |
| 12 | `kill -9` du wrapper : session distante orpheline et secret toujours valide ; un `unregister` ne coupe pas les handshakes en cours | ssh.go:197,241 ; session.go:198-226 | Bail renouvelable lié à une connexion de contrôle ; ramassage des sessions orphelines |
| 13 | Redémarrer le daemon casse toutes les sessions ouvertes (nouveau port, registre vide), alors que la doc dit `pkill` pour appliquer un réglage | main.go:203-221 | Port et registre stables entre redémarrages, ou commande `2o daemon restart` qui le signale ; à défaut documenter |
| 14 | Bootstrap concurrent : deux premières connexions partagent `~/.local/bin/2o.tmp` → une échoue ; le temporaire suit les liens | ssh.go:177-179 | Temporaire unique (`mktemp`) + rename atomique |
| 15 | Après authentification plus aucun délai : transfert interrompu = connexion ouverte à vie ; pas de limite de concurrence ; serveur HTTP sans timeouts ; connexions du pont qui survivent à sa fermeture | session.go:139,226 ; proxy.go:56-137 ; essais : « STILL OPEN » | Délais d'inactivité par phase, `http.Server.Close` + `ReadHeaderTimeout/IdleTimeout`, plafond de connexions |
| 16 | Pont HTTP sérialisé : un flux long (SSE) bloque toutes les autres requêtes | proxy.go:75-125 ; `/health` en timeout pendant un SSE | Un flux par requête (ex. une connexion tunnel par requête), flush du streaming |
| 17 | Pont localhost : échec d'ouverture du navigateur → code 0 et rien d'affiché ; `http://localhost` sans port → « EOF » | main.go:337-402 | Valider avant d'acquitter, remonter l'erreur au client |

## P2 — UX, messages, finitions

- **Aide** : pas de `-h/--help/help` ; `2o --help` ou une faute de frappe sont traités comme un fichier ; `copy --help` copie vraiment ; l'usage omet `status`, les dossiers et `paste -c`, et expose `shell`/`free-port` (main.go:29-143).
- **Latence** : 4 connexions SSH avant le shell (6 la première fois) : 1,9 s contre 0,46 s pour `ssh` direct, 6,7 s à froid, sans indication de progression → une connexion multiplexée (ControlMaster privé) pour la préparation + messages de phase.
- **Opérations silencieuses** : attente de la confirmation (jusqu'à 2 min) sans message ; pont localhost sans « Ctrl-C pour fermer » ; expiration de la confirmation présentée comme un refus.
- **Messages** : `xclip: xclip:` en double ; « content did not stick (compositor refused…) » affirmé sans preuve ; refus de taille sans taille ni limite (`clipboard too large`, `notification too long`) ; erreurs de session qui exposent chemins/ports/erreurs Go avant le conseil ; « local daemon failed to start » sans chemin du log ; « missing SSH option value » sans nommer l'option ; « unknown command » sans la commande.
- **`status`** ne vérifie que le tunnel → ajouter `status -v`/`doctor` : bureau cible, version du daemon, outils détectés, éditeur, politique effective, chemin du log.
- **Shim `xdg-open`** persistant dans `~/.local/bin` après la session → le mettre dans le répertoire de session.
- **Éditeur** : `EDITOR=vim` passe avant un éditeur graphique détecté alors que le daemon n'a pas de terminal.
- **Klipper** reçoit le texte copié en argument (`qdbus … setClipboardContents TEXTE`) → visible dans la liste des processus ; passer par stdin/D-Bus direct.
- **X11** : le premier socket `/tmp/.X11-unix/X*` est choisi sans vérifier son propriétaire (contrairement à Wayland).
- **Redirections** : la réécriture loopback ignore les alias (`127.1`, `[::ffff:127.0.0.1]`…).
- **Noms distants** non neutralisés dans le texte de confirmation et les logs (caractères de contrôle, balisage).
- **Distribution** : architecture différente = lancer depuis le dépôt source avec Go ; publier des binaires amd64/arm64 et documenter le PKGBUILD existant ; quickstart avec un HOST générique.
- **Code** : main.go mélange CLI, client et daemon (576 lignes) ; les tests réimplémentent la boucle du daemon au lieu de l'exercer ; tests dépendants de l'environnement (`TWO_O_CACHE_DAYS=0` fait échouer un test, espace disque réel) ; processus `sleep 30` et pipe non fermés dans les tests ; staticcheck : `tar.TypeRegA` déprécié, message d'erreur capitalisé.
- **Docs** : PLAN.md cite encore le forward Unix comme preuve de validation.

## Features — roadmap proposée

1. **P0** Contrat SSH fiable (parsing, bootstrap/session séparés, PTY, codes de sortie) et routage multi-bureau explicite.
2. **P1** Pont localhost : concurrence et SSE, puis WebSocket/HMR, puis HTTPS avec une politique de certificats explicite.
3. **P1** `--help`, `--version`, `doctor` ; binaires amd64/arm64 vérifiés.
4. **P2** `edit` : création de fichier, ligne/colonne, plusieurs fichiers (comme rmate) en gardant le contrôle de conflit.
5. **P2** Mode `attach` pour une session ssh existante ; alias `pbcopy`/`pbpaste` optionnels ; recette OSC 52 + tmux.
6. **P3** Certifier macOS côté bureau ; Windows si demande réelle.

À ne pas faire sans besoin démontré : synchronisation de répertoires, presse-papiers multi-MIME, lever l'interdiction des exécutables pour « ouvrir un projet ».

## Déjà bien (à ne pas casser)

- Transport délégué à OpenSSH ; secret de session distinct du secret maître, transmis par stdin ; comparaison à temps constant ; un imposteur sur un port périmé ne reçoit rien.
- Politique décidée par le bureau, refus avant tout envoi de données, réserve disque, confirmation.
- Extraction confinée (`os.OpenRoot`), refus des liens, exécutables et doublons, fichiers créés en 0600/0700.
- Séparation `open` (copie) / `edit` (écriture distante) avec copie de récupération en cas de conflit.
- Presse-papiers : plusieurs backends, relecture de vérification, repli tmux ; `stdout` de `paste` toujours propre.
- Essais réels réussis : serveur vierge, Unicode/espaces/fichier vide, 50 MiB (hash identique), lien bridé à 64 KiB/s, pont HTTP, deux sessions en parallèle, nettoyage après fermeture normale.
- `go vet` propre, `-race` sans erreur sur 3 passes ; couverture : files 79 %, proxy 85 %, desktop 59 %, main 39 %.
