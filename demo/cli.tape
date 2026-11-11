# CLI walkthrough: onboarding to running tunnels. Run via demo/record.sh.
Output assets/cli.gif

Set Shell bash
Set FontSize 16
Set Width 1400
Set Height 760
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set WindowBar Colorful

Hide
Type "export PS1='\[\e[1;35m\]❯\[\e[0m\] ' && cd $DEMO_DIR && clear"
Enter
Show

Sleep 500ms
Type "awsport init --from acme.team.yaml"
Sleep 400ms
Enter
Sleep 1.5s

Type "awsport login"
Sleep 400ms
Enter
Sleep 3s
Type "awsport groups"
Sleep 400ms
Enter
Sleep 3.5s

Type "awsport up staging-dev"
Sleep 400ms
Enter
Sleep 4s
Ctrl+C
Sleep 2s
