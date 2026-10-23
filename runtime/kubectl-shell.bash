# JunKloud: exam-style kubectl shortcuts for every interactive bash shell.
# Sourced from /etc/bash.bashrc, so homes on an existing learner-home volume
# get them too; a user's own ~/.bashrc or ~/.bash_aliases can still override.
alias k=kubectl

if ! shopt -oq posix && [ -r /usr/share/bash-completion/bash_completion ]; then
    . /usr/share/bash-completion/bash_completion
fi

# Generated at image build time from the pinned kubectl. Loaded eagerly so
# the `k` alias can reuse kubectl's completion function.
if [ -r /usr/share/bash-completion/completions/kubectl ]; then
    . /usr/share/bash-completion/completions/kubectl
    complete -o default -F __start_kubectl k
fi
