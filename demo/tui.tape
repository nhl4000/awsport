# TUI walkthrough: profile -> resource -> tunnels -> dashboard. Run via demo/record.sh
# after cli.tape (it reuses the profiles that tape synced).
Output assets/tui.gif

Set Shell bash
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 60ms
Set WindowBar Colorful

Hide
Type "export PS1='\[\e[1;35m\]❯\[\e[0m\] ' && cd $DEMO_DIR && clear"
Enter
Show

Sleep 500ms
Type "awsport"
Sleep 300ms
Enter
Sleep 2.5s

# Profiles: pick acme-staging-developer.
Down
Sleep 400ms
Down
Sleep 700ms
Enter
Sleep 2.5s

# Resources: web-app -> mailpit.
Down
Sleep 700ms
Enter
Sleep 1.2s
Down@300ms 3
Sleep 600ms
Enter
Sleep 2s

# Back to resources: app-db (RDS through the tagged bastion).
Escape
Sleep 800ms
Down@300ms 3
Sleep 600ms
Enter
Sleep 2.5s
# Show unreachable resources with their reason.
Type "a"
Sleep 2s
Type "a"
Sleep 500ms

# Tunnel dashboard.
Type "t"
Sleep 3s

# Quit and keep the tunnels running in the background agent.
Type "q"
Sleep 1.8s
Type "k"
Sleep 1.2s

# Reopen: the tunnels are still there.
Type "awsport"
Sleep 300ms
Enter
Sleep 2s
Type "t"
Sleep 2.5s

# Quit and stop everything.
Type "q"
Sleep 1.5s
Type "s"
Sleep 2s
