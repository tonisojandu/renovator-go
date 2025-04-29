package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/google/go-github/v50/github"
	"golang.org/x/oauth2"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	ctx := context.Background()
	var token, tokenVariable, org, user, author, dependency, defaultComment string
	var yes, debug, retryUntilAllMerged bool

	flag.StringVar(&token, "token", "", "GitHub token to use")
	flag.StringVar(&tokenVariable, "token-variable", "", "Name of an environment variable to read GitHub token from")
	flag.StringVar(&org, "o", "", "GitHub organization to renovate")
	flag.StringVar(&user, "u", "", "GitHub user who we are renovating for")
	flag.StringVar(&author, "a", "app/renovate", "The creator of renovate request")
	flag.StringVar(&dependency, "d", "", "The dependency to renovate")
	flag.StringVar(&defaultComment, "m", "LGTM", "The default comment for PR approvals")
	flag.BoolVar(&yes, "y", false, "Approve all matching PR-s")
	flag.BoolVar(&debug, "debug", false, "Enables additional output")
	flag.BoolVar(&retryUntilAllMerged, "retry-until-all-merged", false, "Retry until all PR-s are merged")
	flag.Parse()

	if token == "" && tokenVariable == "" {
		log.Fatal("Either token or token-variable must be provided")
	}

	if token == "" {
		token = os.Getenv(tokenVariable)
		if token == "" {
			log.Fatal("GitHub token is required")
		}
	}

	if org == "" || user == "" {
		log.Fatal("org and user flags are required")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	// Search for PRs
	query := fmt.Sprintf("org:%s author:%s is:open is:pr review-requested:%s", org, author, user)
	searchResult, _, err := client.Search.Issues(ctx, query, nil)
	if err != nil {
		log.Fatalf("Error searching PRs: %v", err)
	}

	fmt.Printf("Found %d renovate PRs for user %s\n", len(searchResult.Issues), user)

	// Filter PRs by dependency if provided
	var matchingPRs []*github.Issue
	if dependency != "" {
		for _, pr := range searchResult.Issues {
			if pr.Title != nil && *pr.Title == dependency {
				if pr.Repository != nil && pr.Repository.Name != nil && *pr.Repository.Name != "" {
					matchingPRs = append(matchingPRs, pr)
					fmt.Printf("Repository details: %+v\n", pr.Repository)
				} else {
					log.Printf("Repository name is missing for PR: %s", *pr.Title)
				}
			}
		}
		fmt.Printf("Found %d renovate PRs for dependency %s\n", len(matchingPRs), dependency)
	} else {
		matchingPRs = searchResult.Issues
		fmt.Printf("Found %d renovate PRs\n", len(matchingPRs))
	}

	// Retry logic
	for {
		// Process each PR
		for _, pr := range matchingPRs {
			fmt.Printf("Processing PR: %s\n", *pr.Title)
			repoUrl := pr.GetHTMLURL()
			fmt.Printf("Repo URL: %s\n", repoUrl)

			// get third last element from the URL into repoName
			repoName := strings.Split(repoUrl, "/")[4]
			if repoName == "" {
				log.Printf("Cannot get repository name for PR: %s", *pr.Title)
				continue
			}
			prDetails, _, err := client.PullRequests.Get(ctx, org, repoName, pr.GetNumber())
			if err != nil {
				log.Printf("Error fetching PR details: %v", err)
				continue
			}
			if prDetails == nil {
				log.Printf("PR details are nil for PR: %s", *pr.Title)
				continue
			}
			if err != nil {
				log.Printf("Error fetching PR details: %v", err)
				continue
			}

			// Get repository details to check if it's archived
			repo, _, err := client.Repositories.Get(ctx, org, repoName)
			if err != nil {
				log.Printf("Error fetching repository details: %v", err)
				continue
			}

			if repo.GetArchived() {
				fmt.Printf("PR %s is in an archived repository, skipping\n", *pr.Title)
				continue
			}

			if prDetails.GetMerged() {
				fmt.Printf("PR %s is already merged\n", *pr.Title)
				continue
			}

			if !prDetails.GetMergeable() {
				fmt.Printf("PR %s cannot be merged\n", *pr.Title)
				continue
			}

			// Check if all checks are successful
			checks, _, err := client.Checks.ListCheckRunsForRef(ctx, org, repoName, prDetails.Head.GetSHA(), nil)
			if err != nil {
				log.Printf("Error fetching check runs: %v", err)
				continue
			}

			allChecksPassed := true
			for _, check := range checks.CheckRuns {
				if check.GetConclusion() != "success" && check.GetConclusion() != "skipped" {
					allChecksPassed = false
					break
				}
			}

			if !allChecksPassed {
				fmt.Printf("PR %s has non-succeeded checks\n", *pr.Title)
				continue
			}

			// Ask for user approval before proceeding
			if confirmMerge(*pr.Title) {
				// Approve the PR
				review := &github.PullRequestReviewRequest{
					Body:  github.String("LGTM"),
					Event: github.String("APPROVE"),
				}
				_, _, err = client.PullRequests.CreateReview(ctx, org, repoName, *pr.Number, review)
				if err != nil {
					log.Printf("Error approving PR: %v", err)
					continue
				}

				// Merge the PR
				options := &github.PullRequestOptions{
					MergeMethod: "rebase",
				}
				_, _, err = client.PullRequests.Merge(ctx, org, repoName, *pr.Number, "", options)
				if err != nil {
					log.Printf("Error merging PR: %v", err)
					continue
				}

				fmt.Printf("Successfully merged PR: %s\n", *pr.Title)
			} else {
				fmt.Printf("Skipping PR: %s\n", *pr.Title)
			}
		}

		// Check if retry is needed
		if !retryUntilAllMerged || allPRsMerged(matchingPRs, client, ctx, org) {
			break
		}

		fmt.Println("Some PR-s are not merged, retrying in 5 seconds")
		time.Sleep(5 * time.Second)
	}
}

func allPRsMerged(prs []*github.Issue, client *github.Client, ctx context.Context, org string) bool {
	for _, pr := range prs {
		repoName := strings.Split(pr.GetHTMLURL(), "/")[4]
		prDetails, _, err := client.PullRequests.Get(ctx, org, repoName, pr.GetNumber())
		if err != nil || prDetails == nil || !prDetails.GetMerged() {
			return false
		}
	}
	return true
}

func confirmMerge(prTitle string) bool {
	var response string
	fmt.Printf("Approve and merge PR '%s'? [y/N]: ", prTitle)
	_, err := fmt.Scanln(&response)
	if err != nil {
		log.Printf("Error reading input: %v", err)
		return false
	}
	switch response {
	case "y", "Y":
		return true
	case "c", "C":
		comment := promptForComment()
		return confirmMergeWithComment(prTitle, comment)
	case "?":
		showInformation()
		return confirmMerge(prTitle)
	default:
		return false
	}
}

func promptForComment() string {
	var comment string
	fmt.Print("Enter comment to approve the PR with: ")
	_, err := fmt.Scanln(&comment)
	if err != nil {
		log.Printf("Error reading comment: %v", err)
		return "LGTM"
	}
	return comment
}

func confirmMergeWithComment(prTitle, comment string) bool {
	fmt.Printf("Approve and merge PR '%s' with comment '%s'? [y/N]: ", prTitle, comment)
	var response string
	_, err := fmt.Scanln(&response)
	if err != nil {
		log.Printf("Error reading input: %v", err)
		return false
	}
	return response == "y" || response == "Y"
}

func showInformation() {
	fmt.Println("y - Approve and merge this PR")
	fmt.Println("n - Skip this PR")
	fmt.Println("c - Approve and merge this PR with custom comment")
	fmt.Println("? - Show this help")
}
