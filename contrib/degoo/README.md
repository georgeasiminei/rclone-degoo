# Local Degoo install cleanup

`uninstall.py` inventories or removes the isolated local setup at
`~/.local/share/rclone-degoo`. It does not change the normal rclone config or
any files on Degoo.

The default command is a read-only inventory. It prints filenames inside the
install root and mount state, but never prints token, vault, status, or log
contents:

```sh
python3 contrib/degoo/uninstall.py --root ~/.local/share/rclone-degoo
```

The contained setup may copy this script to
`~/.local/share/rclone-degoo/uninstall.py`; the same commands work with that
installed path.

To remove the local install, first make a private backup of
`config/vault.json` somewhere outside the install root. The cleanup verifies
that the backup exactly matches the installed file and refuses backups that
are not owner-private regular files. Then run:

```sh
python3 contrib/degoo/uninstall.py \
  --root ~/.local/share/rclone-degoo \
  --delete-local \
  --vault-backup ~/private-backups/degoo-vault.json
```

If you deliberately choose to discard the only local vault secret, replace
`--vault-backup PATH` with `--discard-vault-secret`. The vault configuration
contains the password needed to decrypt the remote encrypted files. Losing
that password makes those remote files unreadable.

Deletion is refused while the isolated mount is active except that the script
may unmount its own contained mount helper, while another mount exists inside
the install root, while the VFS cache lock is held, or while pending local
uploads remain. If setup is incomplete and the rclone config is absent, local
deletion is permitted only when the cache has no files. Keep the isolated
cache until pending uploads reach zero; it can contain the only copy of
unuploaded changes as well as plaintext cache data.

The cleanup removes only the local `~/.local/share/rclone-degoo` directory,
including its account tokens, vault configuration, cache, logs, and wrappers.
Remote encrypted objects in the dedicated Degoo folder are deliberately left
untouched. Remote removal is a separate action and is not part of this
utility.

Run the offline safety tests with:

```sh
python3 -m unittest contrib.degoo.test_uninstall
```
